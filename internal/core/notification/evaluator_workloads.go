package notification

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
)

// The container and unit series the agent's docker.containers and
// service.units collectors write (internal/plugin/docker/collectors.go,
// internal/plugin/service/plugin.go).
const (
	MetricContainerUp       = "docker.container.up"
	MetricContainerRestarts = "docker.container.restarts"
	MetricContainerExitCode = "docker.container.exit_code"
	MetricServiceUp         = "service.up"

	containerLabel      = "container"
	composeProjectLabel = "compose_project"
	composeServiceLabel = "compose_service"
	unitLabel           = "unit"
)

// Baseline keys. Numbers are float64, as they come back from jsonb.
const (
	baselineSeenUp       = "seen_up"
	baselineLastRestarts = "last_restarts"
	// baselineAbsentSince is when the subject was first missing from a fresh
	// metric family, RFC 3339. Removed when the subject is seen again.
	baselineAbsentSince = "absent_since"
)

// vanishedRetention is how long a subject may be absent before its row is
// deleted. It equals storage/metric.LatestRetention, which this package
// cannot import.
const vanishedRetention = 24 * time.Hour

// nodeWorkloads is one node's fresh container and unit samples, by subject.
// An empty map means the family is stale, absent or unreadable: unknown.
type nodeWorkloads struct {
	up, restarts, exitCode, units map[string]MetricSample
}

// readWorkloads reads the fresh container and service samples of every node.
func (e *Evaluator) readWorkloads(ctx context.Context) map[string]*nodeWorkloads {
	out := map[string]*nodeWorkloads{}
	read := func(metric, label string, interval time.Duration, field func(*nodeWorkloads) *map[string]MetricSample) bool {
		if interval <= 0 {
			return true
		}
		samples, err := e.metrics.LatestForMetric(ctx, metric, StaleFactor*interval)
		if err != nil {
			e.logger.ErrorContext(ctx, "could not read a metric for notification evaluation",
				slog.String("metric", metric), slog.String("error", err.Error()))
			return false
		}
		for _, s := range samples {
			subject := s.Labels[label]
			if subject == "" {
				continue
			}
			w := out[s.NodeID]
			if w == nil {
				w = &nodeWorkloads{}
				out[s.NodeID] = w
			}
			m := field(w)
			if *m == nil {
				*m = map[string]MetricSample{}
			}
			// A container recreated under another compose project is a
			// second series with the same name; the newest one is current.
			if old, ok := (*m)[subject]; !ok || s.Time.After(old.Time) {
				(*m)[subject] = s
			}
		}
		return true
	}
	read(MetricContainerUp, containerLabel, e.containerInterval, func(w *nodeWorkloads) *map[string]MetricSample { return &w.up })
	read(MetricContainerRestarts, containerLabel, e.containerInterval, func(w *nodeWorkloads) *map[string]MetricSample { return &w.restarts })
	// After up: see containerExited for why the order matters.
	if !read(MetricContainerExitCode, containerLabel, e.containerInterval, func(w *nodeWorkloads) *map[string]MetricSample { return &w.exitCode }) {
		// Without exit codes every stopped container would look cleanly
		// stopped, and a firing one would recover. Unknown instead.
		for _, w := range out {
			w.up = nil
		}
	}
	read(MetricServiceUp, unitLabel, e.serviceInterval, func(w *nodeWorkloads) *map[string]MetricSample { return &w.units })
	return out
}

// workloadTrigger is one trigger's view of a node: its subjects, and how to
// judge one of them.
type workloadTrigger struct {
	trigger  Trigger
	severity Severity
	family   map[string]MetricSample
	window   time.Duration
	// onChange marks an event trigger: with for_seconds 0 it fires on the
	// tick that observes the change instead of one tick later.
	onChange bool
	judge    func(subject string, sample MetricSample, baseline map[string]any) verdict
}

// verdict is one subject's condition this tick.
type verdict struct {
	condition bool
	value     *float64
	// title announces the condition; message describes what was observed,
	// and so reads correctly for a recovery too.
	title, message string
	// track updates the trigger's own baseline keys given the state this
	// tick arrived at.
	track func(baseline map[string]any, next StateKind)
}

// evaluateWorkloads runs the container and service triggers for one node.
func (e *Evaluator) evaluateWorkloads(ctx context.Context, nodeID string, w *nodeWorkloads, now time.Time) {
	if w == nil {
		return // nothing fresh at all: unknown
	}
	stored, err := e.states.ListNodeStates(ctx, nodeID, TriggerContainerExit, TriggerContainerRestart, TriggerServiceFailed)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not read a node's notification states",
			slog.String("node_id", nodeID), slog.String("error", err.Error()))
		return
	}

	for _, t := range []workloadTrigger{
		{
			trigger: TriggerContainerExit, severity: SeverityCritical, onChange: true,
			family: w.up, window: StaleFactor * e.containerInterval,
			judge: func(subject string, up MetricSample, b map[string]any) verdict {
				return judgeContainerExit(nodeID, subject, up, w.exitCode, b)
			},
		},
		{
			trigger: TriggerContainerRestart, severity: SeverityWarning, onChange: true,
			family: w.restarts, window: StaleFactor * e.containerInterval,
			judge: func(subject string, restarts MetricSample, b map[string]any) verdict {
				return judgeContainerRestart(nodeID, subject, restarts, b)
			},
		},
		{
			trigger: TriggerServiceFailed, severity: SeverityCritical,
			family: w.units, window: StaleFactor * e.serviceInterval,
			judge: func(subject string, up MetricSample, b map[string]any) verdict {
				return judgeServiceFailed(nodeID, subject, up, b)
			},
		},
	} {
		if len(t.family) == 0 {
			continue // stale or absent: unknown, so neither judged nor vanished
		}
		states := map[string]State{}
		for _, s := range stored {
			if s.Trigger == t.trigger {
				states[s.Subject] = s
			}
		}
		e.evaluateTrigger(ctx, t, nodeID, states, now)
		e.sweepVanished(ctx, t, states, now)
	}
}

func (e *Evaluator) evaluateTrigger(ctx context.Context, t workloadTrigger, nodeID string, states map[string]State, now time.Time) {
	rule, err := e.rules.EffectiveRule(ctx, t.trigger, nodeID)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not resolve a notification rule",
			slog.String("trigger", string(t.trigger)), slog.String("node_id", nodeID))
		return
	}

	for _, subject := range slices.Sorted(maps.Keys(t.family)) {
		current, found := states[subject]
		v := t.judge(subject, t.family[subject], current.Baseline)
		track := func(next StateKind) map[string]any {
			b := maps.Clone(current.Baseline)
			if b == nil {
				b = map[string]any{}
			}
			delete(b, baselineAbsentSince)
			v.track(b, next)
			return b
		}

		if !rule.Enabled || slices.Contains(rule.SubjectExcludes, subject) {
			// No transition, but the baseline keeps following reality:
			// restarts counted while the rule was off are not news when it
			// is turned back on.
			e.saveBaseline(ctx, t.trigger, nodeID, subject, current, found, track(StateOK), now)
			continue
		}

		severityOf := func(*float64) Severity { return t.severity }
		if t.onChange && rule.ForSeconds == 0 && v.condition && (!found || current.State == StateOK) {
			// The pending state is stored before it is acted on, with the
			// baseline untouched, so the firing that follows on this same
			// tick is anchored on a persisted ConditionSince: a retry after
			// a crash, or a second evaluator, builds the same event id.
			pending, _, _ := computeTransition(rule, current, found, true, v.value, severityOf, now)
			pending.Trigger, pending.NodeID, pending.Subject = t.trigger, nodeID, subject
			begun, err := e.states.BeginPending(ctx, pending)
			if err != nil {
				e.logger.ErrorContext(ctx, "could not save notification state",
					slog.String("trigger", string(t.trigger)), slog.String("node_id", nodeID))
				continue
			}
			current, found = begun, true
		}
		e.evaluateSubjectWithTitle(ctx, rule, t.trigger, nodeID, subject, current, found, v.condition, v.value, severityOf, now,
			func(Severity) (string, string) { return v.title, v.message }, track)
	}
}

// saveBaseline stores baseline on an otherwise unchanged row.
func (e *Evaluator) saveBaseline(
	ctx context.Context, trigger Trigger, nodeID, subject string, current State, found bool, baseline map[string]any, now time.Time,
) {
	next := current
	if !found {
		next = State{Trigger: trigger, NodeID: nodeID, Subject: subject, State: StateOK, StateSince: now}
	}
	next.Baseline, next.UpdatedAt = baseline, now
	if sameState(current, next) || (!found && len(baseline) == 0) {
		return
	}
	e.saveWorkloadState(ctx, next)
}

func (e *Evaluator) saveWorkloadState(ctx context.Context, s State) {
	if err := e.states.UpsertState(ctx, s); err != nil {
		e.logger.ErrorContext(ctx, "could not save notification state",
			slog.String("trigger", string(s.Trigger)), slog.String("node_id", s.NodeID))
	}
}

// sweepVanished handles rows whose subject is missing from a family that is
// otherwise fresh on this node, which means the container or unit was
// removed. After the staleness window a non-ok row goes back to ok, and after
// vanishedRetention the row is deleted; neither sends anything. The baseline
// survives the reset, so a subject that comes back is still compared with
// what was last known of it.
func (e *Evaluator) sweepVanished(ctx context.Context, t workloadTrigger, states map[string]State, now time.Time) {
	for subject, s := range states {
		if _, present := t.family[subject]; present {
			continue
		}
		since, ok := absentSince(s.Baseline)
		if !ok {
			s.Baseline = maps.Clone(s.Baseline)
			if s.Baseline == nil {
				s.Baseline = map[string]any{}
			}
			s.Baseline[baselineAbsentSince] = now.UTC().Format(time.RFC3339Nano)
			s.UpdatedAt = now
			e.saveWorkloadState(ctx, s)
			continue
		}

		absent := now.Sub(since)
		switch {
		case absent > vanishedRetention:
			if err := e.states.DeleteState(ctx, s.Trigger, s.NodeID, s.Subject); err != nil {
				e.logger.ErrorContext(ctx, "could not delete a vanished subject's notification state",
					slog.String("trigger", string(s.Trigger)), slog.String("node_id", s.NodeID))
			}
		case absent > t.window && s.State != StateOK:
			s.State, s.StateSince, s.Severity, s.Value, s.ConditionSince = StateOK, now, "", nil, nil
			s.FlapCount, s.FlapWindowStart, s.UpdatedAt = 0, nil, now
			e.saveWorkloadState(ctx, s)
		}
	}
}

func absentSince(baseline map[string]any) (time.Time, bool) {
	raw, _ := baseline[baselineAbsentSince].(string)
	at, err := time.Parse(time.RFC3339Nano, raw)
	return at, err == nil
}

func seenUp(baseline map[string]any) bool {
	v, _ := baseline[baselineSeenUp].(bool)
	return v
}

// trackSeenUp records that a subject has been observed up, permanently.
func trackSeenUp(up bool) func(map[string]any, StateKind) {
	return func(b map[string]any, _ StateKind) {
		if up {
			b[baselineSeenUp] = true
		}
	}
}

func judgeContainerExit(nodeID, subject string, up MetricSample, exitCodes map[string]MetricSample, b map[string]any) verdict {
	name := containerName(subject, up.Labels)
	v := verdict{title: "Container exited", track: trackSeenUp(up.Value != 0)}

	code, exited := containerExited(subject, up, exitCodes)
	switch {
	case exited:
		v.condition, v.value = seenUp(b), &code
		v.message = fmt.Sprintf("%s on %s exited with code %s", name, nodeID, exitCodeText(int(code)))
	case up.Value != 0:
		v.message = fmt.Sprintf("%s on %s is running", name, nodeID)
	default:
		v.message = fmt.Sprintf("%s on %s is stopped with no error exit code", name, nodeID)
	}
	return v
}

// containerExited reports the non-zero code of a stopped container.
//
// The collector writes exit_code only for a container that is stopped with a
// non-zero code, in the same run and with the same timestamp as up. An
// exit_code older than up is therefore left over from an earlier run — a
// crash, a restart, then a clean stop, all inside the staleness window — and
// must not be read as why the container is stopped now. Reading up before
// exit_code keeps an ingest between the two reads from making a current code
// look old.
func containerExited(subject string, up MetricSample, exitCodes map[string]MetricSample) (float64, bool) {
	exit, ok := exitCodes[subject]
	if up.Value != 0 || !ok || exit.Value == 0 || exit.Time.Before(up.Time) {
		return 0, false
	}
	return exit.Value, true
}

func judgeContainerRestart(nodeID, subject string, restarts MetricSample, b map[string]any) verdict {
	name := containerName(subject, restarts.Labels)
	total := restarts.Value
	last, known := b[baselineLastRestarts].(float64)
	v := verdict{
		title: "Container restarted", value: &total,
		message:   fmt.Sprintf("%s on %s has not restarted again (%.0f in total)", name, nodeID, total),
		condition: known && total > last,
	}
	if v.condition {
		v.message = fmt.Sprintf("%s on %s restarted %.0f time(s) since the previous observation (%.0f in total)",
			name, nodeID, total-last, total)
	}
	v.track = func(b map[string]any, next StateKind) {
		// While pending, the increase has not been announced yet. Moving the
		// baseline now would make the condition false on the next tick and
		// lose it.
		if v.condition && next == StatePending {
			return
		}
		b[baselineLastRestarts] = total
	}
	return v
}

// judgeServiceFailed: service.up is 1 only for an active, running unit, so 0
// cannot tell failed from stopped, inactive or never started. Requiring that
// the unit was once observed up is what keeps units that are simply not meant
// to run from alerting; a unit stopped on purpose still does.
func judgeServiceFailed(nodeID, subject string, up MetricSample, b map[string]any) verdict {
	v := verdict{title: "Service down", track: trackSeenUp(up.Value != 0)}
	if up.Value != 0 {
		v.message = fmt.Sprintf("%s on %s is running", subject, nodeID)
		return v
	}
	v.condition = seenUp(b)
	v.message = fmt.Sprintf("%s on %s is not running (failed or stopped)", subject, nodeID)
	return v
}

// containerName is a container's display name with its compose identity.
func containerName(name string, labels map[string]string) string {
	var parts []string
	if p := labels[composeProjectLabel]; p != "" {
		parts = append(parts, "project "+p)
	}
	if s := labels[composeServiceLabel]; s != "" {
		parts = append(parts, "service "+s)
	}
	if len(parts) == 0 {
		return "container " + name
	}
	return fmt.Sprintf("container %s (compose %s)", name, strings.Join(parts, ", "))
}

func exitCodeText(code int) string {
	switch code {
	case 137:
		return "137 (killed: usually out of memory or a stop timeout)"
	case 143:
		return "143 (SIGTERM)"
	default:
		return fmt.Sprint(code)
	}
}

// sameState reports whether writing next over current would change nothing
// but updated_at.
func sameState(current, next State) bool {
	if len(current.Baseline) == 0 && len(next.Baseline) == 0 {
		current.Baseline, next.Baseline = nil, nil
	}
	return sameTime(&current.StateSince, &next.StateSince) &&
		sameTime(current.ConditionSince, next.ConditionSince) &&
		sameTime(current.LastNotifiedAt, next.LastNotifiedAt) &&
		sameTime(current.FlapWindowStart, next.FlapWindowStart) &&
		current.State == next.State && current.Severity == next.Severity &&
		current.LastNotifiedKind == next.LastNotifiedKind && current.FlapCount == next.FlapCount &&
		reflect.DeepEqual(current.Value, next.Value) && reflect.DeepEqual(current.Baseline, next.Baseline)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
