package notification

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"
)

// EvalMonitor is what the evaluator needs of one website monitor: its
// alerting settings and the prober's current verdict. Down already has the
// monitor's retries applied, and up its recovery successes.
type EvalMonitor struct {
	MonitorID        string
	Active           bool
	AlertsMutedUntil *time.Time
	// Status is the prober's: unknown, up, pending or down.
	Status         string
	StatusSince    time.Time
	DownSince      *time.Time
	LastFailure    string
	LastStatusCode *int
	LastResponseMS *int
	CertNotify     bool
	CertWarnDays   int
	CertCritDays   int
	CertExpiresAt  *time.Time
}

// MonitorSource lists every website monitor for evaluation.
type MonitorSource interface {
	ListEvalMonitors(ctx context.Context) ([]EvalMonitor, error)
}

// MonitorStateStore is the monitor half of the evaluator's memory. States
// are written through [StateStore.UpsertState] and [StateStore.BeginPending]
// like a node's, with MonitorID set.
type MonitorStateStore interface {
	GetMonitorState(ctx context.Context, trigger Trigger, monitorID, subject string) (State, bool, error)
	// ListActiveMonitorStates returns every monitor state that is not ok.
	ListActiveMonitorStates(ctx context.Context) ([]State, error)
	// ResetMonitorStates silently returns every non-ok state of the monitor
	// to ok.
	ResetMonitorStates(ctx context.Context, monitorID string) error
	// EndMonitorMuteAfterExpiry is [StateStore.EndMuteAfterExpiry] for a
	// monitor.
	EndMonitorMuteAfterExpiry(ctx context.Context, monitorID string, now time.Time) (did bool, err error)
}

// Monitor statuses the evaluator reads.
const monitorStatusDown = "down"

// baselineDownSince keeps when the prober marked the monitor down, so the
// recovery can say how long the outage lasted.
const baselineDownSince = "down_since"

func (e *Evaluator) evaluateMonitors(ctx context.Context, rules ruleSet) {
	if e.monitors == nil || e.monitorStates == nil {
		return
	}
	monitors, err := e.monitors.ListEvalMonitors(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not list website monitors for notification evaluation", slog.String("error", err.Error()))
		return
	}
	activeStates, err := e.monitorStates.ListActiveMonitorStates(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not read active website notification states", slog.String("error", err.Error()))
		return
	}
	active := map[string][]State{}
	for _, s := range activeStates {
		active[s.MonitorID] = append(active[s.MonitorID], s)
	}
	offline := e.probeOffline != nil && e.probeOffline()
	now := e.now().Truncate(time.Microsecond)
	for _, m := range monitors {
		e.evaluateMonitor(ctx, rules, m, active[m.MonitorID], offline, now)
	}
}

func (e *Evaluator) evaluateMonitor(ctx context.Context, rules ruleSet, m EvalMonitor, active []State, offline bool, now time.Time) {
	if !m.Active {
		// Paused: nothing is checked, so nothing may stay firing. Silent,
		// like archiving a node.
		if len(active) > 0 {
			if err := e.monitorStates.ResetMonitorStates(ctx, m.MonitorID); err != nil {
				e.logger.ErrorContext(ctx, "could not reset a paused website's notification state",
					slog.String("monitor_id", m.MonitorID), slog.String("error", err.Error()))
			}
		}
		return
	}
	if m.AlertsMutedUntil != nil {
		if m.AlertsMutedUntil.After(now) {
			return
		}
		did, err := e.monitorStates.EndMonitorMuteAfterExpiry(ctx, m.MonitorID, now)
		if err != nil {
			e.logger.ErrorContext(ctx, "could not end a website's expired mute; skipping it this tick",
				slog.String("monitor_id", m.MonitorID), slog.String("error", err.Error()))
			return
		}
		if !did {
			return
		}
		active = nil // reset by the mute's end
	}

	scope := MonitorScope(m.MonitorID)
	for _, s := range active {
		rule, err := rules.effective(s.Trigger, scope)
		if err != nil || (rule.Enabled && (s.Trigger != TriggerWebsiteCertExpiry || m.CertNotify)) {
			continue
		}
		if next, changed := disabledReset(s, now); changed {
			if err := e.states.UpsertState(ctx, next); err != nil {
				e.logger.ErrorContext(ctx, "could not reset a disabled rule's notification state",
					slog.String("trigger", string(s.Trigger)), slog.String("monitor_id", m.MonitorID))
			}
		}
	}

	// The prober holds every status while it has no connectivity of its own;
	// a down it recorded just before noticing must not page anyone either,
	// nor may a recovery be announced from a verdict that cannot be trusted.
	if !offline {
		e.evaluateWebsiteDown(ctx, rules, m, now)
	}
	e.evaluateCertExpiry(ctx, rules, m, now)
}

func (e *Evaluator) evaluateWebsiteDown(ctx context.Context, rules ruleSet, m EvalMonitor, now time.Time) {
	rule, err := rules.effective(TriggerWebsiteDown, MonitorScope(m.MonitorID))
	if err != nil {
		e.logger.ErrorContext(ctx, "could not resolve the website_down rule", slog.String("monitor_id", m.MonitorID))
		return
	}
	if !rule.Enabled {
		return
	}
	down := m.Status == monitorStatusDown
	ref := subjectRef{MonitorID: m.MonitorID, details: func(kind Kind, current State) MonitorDetails {
		d := MonitorDetails{StatusCode: m.LastStatusCode, ResponseMS: m.LastResponseMS}
		if kind != KindRecovered {
			d.Failure, d.DownSince = m.LastFailure, m.DownSince
			return d
		}
		if since, ok := baselineTime(current.Baseline, baselineDownSince); ok {
			d.DownSince = &since
			end := m.StatusSince
			if end.Before(since) {
				end = now
			}
			secs := int(end.Sub(since).Seconds())
			d.OutageSeconds = &secs
		}
		return d
	}}
	track := func(next StateKind) map[string]any {
		if next == StateOK || next == StatePending {
			return nil
		}
		if m.DownSince != nil {
			return map[string]any{baselineDownSince: m.DownSince.UTC().Format(time.RFC3339Nano)}
		}
		return nil
	}
	e.evaluateMonitorSubject(ctx, rule, TriggerWebsiteDown, m, ref, down, nil,
		func(*float64) Severity { return SeverityCritical }, now,
		func(kind Kind, _ Severity) (string, string) {
			if kind == KindRecovered {
				return "Website down", "The website is up again."
			}
			return "Website down", "The website is down: " + failureText(m.LastFailure) + "."
		}, track)
}

func (e *Evaluator) evaluateCertExpiry(ctx context.Context, rules ruleSet, m EvalMonitor, now time.Time) {
	if !m.CertNotify || m.CertExpiresAt == nil {
		return // not watched, or no certificate seen yet: nothing to judge
	}
	rule, err := rules.effective(TriggerWebsiteCertExpiry, MonitorScope(m.MonitorID))
	if err != nil {
		e.logger.ErrorContext(ctx, "could not resolve the website_cert_expiry rule", slog.String("monitor_id", m.MonitorID))
		return
	}
	if !rule.Enabled {
		return
	}
	expires := *m.CertExpiresAt
	remaining := expires.Sub(now)
	days := CertDaysLeft(expires, now)
	value := float64(days)
	expiring := remaining <= time.Duration(m.CertWarnDays)*24*time.Hour
	severityOf := func(*float64) Severity {
		if remaining <= time.Duration(m.CertCritDays)*24*time.Hour {
			return SeverityCritical
		}
		return SeverityWarning
	}
	ref := subjectRef{MonitorID: m.MonitorID, details: func(Kind, State) MonitorDetails {
		at := expires.UTC()
		return MonitorDetails{CertExpiresAt: &at, CertDaysLeft: &days}
	}}
	e.evaluateMonitorSubject(ctx, rule, TriggerWebsiteCertExpiry, m, ref, expiring, &value, severityOf, now,
		func(kind Kind, _ Severity) (string, string) {
			date := expires.UTC().Format("2006-01-02")
			switch {
			case kind == KindRecovered:
				return "Certificate expiring", "The certificate was renewed; it now expires on " + date + "."
			case remaining <= 0:
				return "Certificate expired", "The certificate expired on " + date + "."
			default:
				return "Certificate expiring", fmt.Sprintf("The certificate expires in %s, on %s.", dayText(days), date)
			}
		}, nil)
}

// evaluateMonitorSubject runs the shared state machine for one monitor
// trigger. Both monitor rules are seeded with zero holds, because the
// prober has already confirmed what it reports; a zero hold is taken on
// the tick that observes the change, not one tick later. Each
// intermediate state is stored before it is acted on, so the transition
// that follows on the same tick is anchored on a persisted timestamp: a
// retry after a crash, or a second evaluator, builds the same event id.
func (e *Evaluator) evaluateMonitorSubject(
	ctx context.Context, rule Rule, trigger Trigger, m EvalMonitor, ref subjectRef, condition bool, value *float64,
	severityOf func(*float64) Severity, now time.Time, titleMessage func(Kind, Severity) (string, string),
	track func(StateKind) map[string]any,
) {
	current, found, err := e.monitorStates.GetMonitorState(ctx, trigger, m.MonitorID, "")
	if err != nil {
		e.logger.ErrorContext(ctx, "could not read notification state",
			slog.String("trigger", string(trigger)), slog.String("monitor_id", m.MonitorID))
		return
	}
	stage := func(s State) State {
		s.Trigger, s.MonitorID, s.NodeID, s.Subject, s.UpdatedAt = trigger, m.MonitorID, "", "", now
		return s
	}
	switch {
	case condition && rule.ForSeconds == 0 && (!found || current.State == StateOK):
		pending, _, _ := computeTransition(rule, current, found, true, value, severityOf, now)
		begun, err := e.states.BeginPending(ctx, stage(pending))
		if err != nil {
			e.logger.ErrorContext(ctx, "could not save notification state",
				slog.String("trigger", string(trigger)), slog.String("monitor_id", m.MonitorID))
			return
		}
		current, found = begun, true
	case !condition && rule.RecoverySeconds == 0 && found && current.State == StateFiring:
		recovering, _, _ := computeTransition(rule, current, true, false, value, severityOf, now)
		recovering = stage(recovering)
		if err := e.states.UpsertState(ctx, recovering); err != nil {
			e.logger.ErrorContext(ctx, "could not save notification state",
				slog.String("trigger", string(trigger)), slog.String("monitor_id", m.MonitorID))
			return
		}
		current = recovering
	}
	var trackFn func(StateKind) map[string]any
	if track != nil {
		trackFn = func(next StateKind) map[string]any {
			if b := track(next); b != nil {
				return b
			}
			if next == StateRecovering || next == StateFiring {
				return current.Baseline
			}
			return nil
		}
	}
	e.evaluateSubjectWithTitle(ctx, rule, trigger, ref, "", current, found, condition, value, severityOf, now, titleMessage, trackFn)
}

// CertDaysLeft is the whole days until expires, rounded down; negative once
// it has passed.
func CertDaysLeft(expires, now time.Time) int {
	return int(math.Floor(expires.Sub(now).Hours() / 24))
}

func dayText(days int) string {
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

func baselineTime(baseline map[string]any, key string) (time.Time, bool) {
	raw, _ := baseline[key].(string)
	t, err := time.Parse(time.RFC3339Nano, raw)
	return t, err == nil
}

// failureText is a monitor failure category as a reader sees it.
func failureText(failure string) string {
	switch failure {
	case "dns":
		return "the host name does not resolve"
	case "connect":
		return "the connection was refused or failed"
	case "tls":
		return "the TLS handshake failed"
	case "timeout":
		return "the request timed out"
	case "http_status":
		return "it answered with an unexpected status code"
	case "keyword":
		return "the expected keyword is missing from the response"
	case "redirect":
		return "it redirected too many times"
	case "blocked":
		return "its address is not allowed"
	default:
		return "the check failed"
	}
}
