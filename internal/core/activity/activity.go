// Package activity keeps a short, human-facing record of notable things that
// have happened, for the "recent activity" view an operator glances at.
//
// # Why this is in memory and not in Postgres
//
// The event bus is deliberately lossy (see internal/platform/eventbus): it
// drops rather than blocks, because a monitoring system must never slow down
// the thing it monitors. A consumer that genuinely cannot lose events has to
// persist and reconcile, and that consumer already exists in the plan — the
// incident timeline, which correlates events, groups them into incidents, and
// retains them for as long as an investigation might need.
//
// This is not that. This is the glanceable feed: "what has happened since
// Atlas came up". A bounded ring buffer is exactly the right shape for it —
// it costs no schema, no write amplification on the ingest path, and it is
// bounded by construction rather than by a retention policy that has to be
// tuned. Building it on Postgres now would fix a schema in place before the
// timeline's requirements are known, which is the more expensive mistake.
//
// The consequence is honest and must be surfaced, not hidden: a restart
// empties the feed, and the API says so.
package activity

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

// Severity is how much an entry should draw the eye.
type Severity string

const (
	// SeverityInfo is something that happened and is worth seeing, but is not
	// a problem — a container stopping on purpose, a hostname changing.
	SeverityInfo Severity = "info"
	// SeveritySuccess is a recovery: something that was wrong is now right.
	SeveritySuccess Severity = "success"
	// SeverityWarning is a problem that has not yet caused data loss.
	SeverityWarning Severity = "warning"
	// SeverityDanger is a failure, or something actively losing observations.
	SeverityDanger Severity = "danger"
)

// Entry is one recorded occurrence, already reduced to what a feed renders.
//
// The payload is deliberately not carried through. An activity feed is a
// human surface, and keeping a reference to a plugin's event struct would
// both retain memory the ring buffer cannot bound and drag plugin types into
// the API layer.
type Entry struct {
	ID       string         `json:"id"`
	Time     time.Time      `json:"time"`
	Topic    eventbus.Topic `json:"topic"`
	Severity Severity       `json:"severity"`
	// Title is the headline: "Container stopped".
	Title string `json:"title"`
	// Detail names the thing it happened to: "nginx-proxy · exit 137".
	Detail string `json:"detail,omitempty"`
	// Source is the component that observed it, for tracing an entry back.
	Source string `json:"source,omitempty"`
}

// Describer is implemented by event payloads that can describe themselves for
// the activity feed.
//
// This interface is the reason this package needs no knowledge of Docker,
// systemd, or anything else that publishes. A plugin already depends on core,
// so it can implement this; core must never import a plugin, so it cannot
// type-assert on one. Payloads that do not implement it still appear, titled
// from their topic — the feed degrades to less detail rather than dropping
// the event.
type Describer interface {
	// ActivityDescription returns the headline and the detail line.
	ActivityDescription() (title, detail string)
}

// notable maps the topics worth showing to their severity.
//
// Everything absent from this map is deliberately not recorded. The excluded
// set is mostly routine churn — image pulls, volume and network creation,
// container create/destroy — which on a CI host or a developer's machine runs
// to thousands an hour and would bury the handful of entries that matter. The
// same reasoning the Docker plugin uses to decide what is worth charting.
var notable = map[eventbus.Topic]Severity{
	"docker.container.started":        SeveritySuccess,
	"docker.container.stopped":        SeverityInfo,
	"docker.container.died":           SeverityWarning,
	"docker.container.oom":            SeverityDanger,
	"docker.container.health_changed": SeverityWarning,

	"collector.run.failed":           SeverityWarning,
	"collector.run.panicked":         SeverityDanger,
	"collector.run.recovered":        SeveritySuccess,
	"collector.cardinality.exceeded": SeverityWarning,

	"system.host.rebooted": SeverityWarning,
	"system.host.renamed":  SeverityInfo,
}

// Classify reports the severity of a topic and whether it is notable at all.
//
// Exported so other consumers of the notable set — the incident timeline's
// correlator, in particular — classify a topic identically to the activity
// feed rather than keeping a second, driftable copy of this table.
func Classify(topic eventbus.Topic) (Severity, bool) {
	s, ok := notable[topic]
	return s, ok
}

// TitleFor returns the human phrase for a topic, falling back to the topic
// itself for one this package has no phrase for.
func TitleFor(topic eventbus.Topic) string {
	if t, ok := titles[topic]; ok {
		return t
	}
	return string(topic)
}

// titles are the human phrases for the topics above.
var titles = map[eventbus.Topic]string{
	"docker.container.started":        "Container started",
	"docker.container.stopped":        "Container stopped",
	"docker.container.died":           "Container exited",
	"docker.container.oom":            "Container out of memory",
	"docker.container.health_changed": "Container health changed",

	"collector.run.failed":           "Collector failed",
	"collector.run.panicked":         "Collector panicked",
	"collector.run.recovered":        "Collector recovered",
	"collector.cardinality.exceeded": "Series limit reached",

	"system.host.rebooted": "Host rebooted",
	"system.host.renamed":  "Hostname changed",
}

// DefaultCapacity is how many entries are kept. Sized for a feed someone
// scrolls, not an archive: older than this belongs to the incident timeline.
const DefaultCapacity = 200

// Recorder subscribes to the event bus and keeps the most recent notable
// entries.
//
// It is a lifecycle component: Start subscribes and begins draining, Stop
// unsubscribes and ends the drain. Reads are safe from any goroutine.
type Recorder struct {
	capacity int
	bus      *eventbus.Bus
	logger   *slog.Logger

	mu      sync.RWMutex
	entries []Entry // newest last
	since   time.Time

	sub  *eventbus.Subscription
	done chan struct{}
	wg   sync.WaitGroup
}

// Options configures a [Recorder].
type Options struct {
	// Bus is the event source. Required.
	Bus *eventbus.Bus
	// Capacity is the ring size. Zero selects [DefaultCapacity].
	Capacity int
	Logger   *slog.Logger
}

// New builds a Recorder. It does not subscribe until Start.
func New(opts Options) *Recorder {
	if opts.Capacity <= 0 {
		opts.Capacity = DefaultCapacity
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Recorder{
		capacity: opts.Capacity,
		bus:      opts.Bus,
		logger:   opts.Logger,
		entries:  make([]Entry, 0, opts.Capacity),
	}
}

// Name implements [lifecycle.Component].
func (r *Recorder) Name() string { return "activity.recorder" }

// Start subscribes to the bus and begins recording.
func (r *Recorder) Start(context.Context) error {
	const op = "activity.Recorder.Start"

	if r.bus == nil {
		return errs.New(errs.CodeInvalidArgument, "activity recorder requires an event bus").WithOp(op)
	}

	// Subscribed to everything, filtered here rather than by pattern: the
	// notable set spans several unrelated topic prefixes, and one subscriber
	// with one queue is cheaper than four subscriptions each with their own.
	sub, err := r.bus.Subscribe("core.activity", eventbus.MatchAll)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not subscribe to the event bus").WithOp(op)
	}

	r.mu.Lock()
	r.sub = sub
	r.since = time.Now()
	r.done = make(chan struct{})
	r.mu.Unlock()

	r.wg.Add(1)
	go r.drain(sub, r.done)
	return nil
}

// Stop unsubscribes and waits for the drain to finish.
func (r *Recorder) Stop(context.Context) error {
	r.mu.Lock()
	sub, done := r.sub, r.done
	r.sub, r.done = nil, nil
	r.mu.Unlock()

	if sub == nil {
		return nil
	}
	close(done)
	sub.Close()
	r.wg.Wait()
	return nil
}

func (r *Recorder) drain(sub *eventbus.Subscription, done <-chan struct{}) {
	defer r.wg.Done()
	for {
		select {
		case <-done:
			return
		case event, open := <-sub.C:
			if !open {
				return
			}
			r.record(event)
		}
	}
}

// record appends one event if its topic is notable.
func (r *Recorder) record(e eventbus.Event) {
	severity, ok := notable[e.Topic]
	if !ok {
		return
	}

	title := titles[e.Topic]
	if title == "" {
		title = string(e.Topic)
	}
	detail := e.Subject

	// A payload that can describe itself gets to; anything else falls back to
	// the topic phrase and the subject, which are always present.
	if d, isDescriber := e.Payload.(Describer); isDescriber {
		if t, dt := d.ActivityDescription(); t != "" {
			title, detail = t, dt
		}
	}

	entry := Entry{
		ID: e.ID, Time: e.Time, Topic: e.Topic,
		Severity: severity, Title: title, Detail: detail, Source: e.Source,
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == r.capacity {
		// Drop the oldest. Copying rather than reslicing keeps the backing
		// array from growing without bound as entries churn.
		copy(r.entries, r.entries[1:])
		r.entries[len(r.entries)-1] = entry
		return
	}
	r.entries = append(r.entries, entry)
}

// Recent returns up to limit entries, newest first.
func (r *Recorder) Recent(limit int) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if limit <= 0 || limit > len(r.entries) {
		limit = len(r.entries)
	}

	out := make([]Entry, 0, limit)
	for i := len(r.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, r.entries[i])
	}
	return out
}

// Since reports when this recorder started listening.
//
// The API returns it so a client can say "since Atlas started" rather than
// implying the feed is a complete history. An empty feed on a long-running
// instance means nothing notable happened; an empty feed one minute after a
// restart means nothing has happened *yet*, and those are different.
func (r *Recorder) Since() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.since
}

// Dropped reports how many events the bus discarded for this subscriber
// because it could not keep up. Non-zero means the feed has gaps.
func (r *Recorder) Dropped() uint64 {
	r.mu.RLock()
	sub := r.sub
	r.mu.RUnlock()
	if sub == nil {
		return 0
	}
	return sub.Dropped()
}
