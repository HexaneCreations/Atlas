package monitor

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Due is a claimed monitor whose check is due.
type Due struct {
	Monitor  Monitor
	Secret   Secret
	Counters Counters
	// SecretUnreadable: the stored secret cannot be decrypted (a different
	// key, or none configured). The check is skipped, not failed.
	SecretUnreadable bool
}

// CheckRecord is everything one check writes, in one transaction.
type CheckRecord struct {
	MonitorID     string
	At            time.Time
	Counters      Counters
	Heartbeat     Status
	Result        Result
	Event         *Event
	NextCheckAt   time.Time
	CertExpiresAt *time.Time
}

// Store is the scheduler's persistence.
type Store interface {
	// SpreadOverdue reschedules every overdue active monitor to a random
	// moment within min(interval, maxSpread) from now.
	SpreadOverdue(ctx context.Context, maxSpread time.Duration) (int, error)
	// ClaimDue claims up to limit due active monitors, pushing each one's
	// next_check_at lease into the future so no other claim takes it.
	ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]Due, error)
	// RecordCheck writes rec, unless the monitor was deleted or paused
	// meanwhile.
	RecordCheck(ctx context.Context, rec CheckRecord) error
	// Reschedule sets a monitor's next check without recording one.
	Reschedule(ctx context.Context, monitorID string, at time.Time) error
	// PruneEvents deletes events older than before.
	PruneEvents(ctx context.Context, before time.Time) (int64, error)
}

// Checker runs one check; [*Prober] in production.
type Checker interface {
	Check(ctx context.Context, m Monitor, s Secret) Result
}

// SchedulerOptions configures a [Scheduler].
type SchedulerOptions struct {
	Store          Store
	Checker        Checker
	Outage         *OutageDetector
	Logger         *slog.Logger
	MaxConcurrent  int
	Poll           time.Duration // default 1s
	StartupSpread  time.Duration // default 30s
	EventRetention time.Duration // default 365 days
	Now            func() time.Time
}

// Scheduler claims due monitors and runs their checks with bounded
// concurrency. State lives in the store, so a restart resumes where it
// left off: counters survive, overdue checks are spread out instead of
// all firing at once, and missed checks are not backfilled. Claims are
// leases, so several servers sharing a database never check one monitor
// twice at the same time.
type Scheduler struct {
	o        SchedulerOptions
	sem      chan struct{}
	wg       sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
	running  atomic.Bool
	done     chan struct{}
}

// NewScheduler builds a scheduler; Run starts it.
func NewScheduler(o SchedulerOptions) *Scheduler {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 16
	}
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.StartupSpread <= 0 {
		o.StartupSpread = 30 * time.Second
	}
	if o.EventRetention <= 0 {
		o.EventRetention = 365 * 24 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Outage == nil {
		o.Outage = NewOutageDetector(nil)
	}
	return &Scheduler{o: o, sem: make(chan struct{}, o.MaxConcurrent), stop: make(chan struct{}), done: make(chan struct{})}
}

// ProbeState reports the outage detector's verdict.
func (s *Scheduler) ProbeState() ProbeState { return s.o.Outage.State() }

// Run loops until ctx ends or Stop is called.
func (s *Scheduler) Run(ctx context.Context) {
	s.running.Store(true)
	defer close(s.done)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	if n, err := s.o.Store.SpreadOverdue(ctx, s.o.StartupSpread); err != nil {
		s.o.Logger.ErrorContext(ctx, "could not reschedule overdue website checks", slog.String("error", err.Error()))
	} else if n > 0 {
		s.o.Logger.InfoContext(ctx, "rescheduled overdue website checks", slog.Int("monitors", n))
	}
	s.prune(ctx)

	poll := time.NewTicker(s.o.Poll)
	defer poll.Stop()
	prune := time.NewTicker(24 * time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-prune.C:
			s.prune(ctx)
		case <-poll.C:
			s.RunDue(ctx)
		}
	}
}

// Stop ends Run and waits for in-flight checks.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.running.Load() {
		<-s.done
	}
}

// RunDue claims whatever is due and fits in the free concurrency, and
// starts those checks. Exported for tests, which call it directly and then
// Wait.
func (s *Scheduler) RunDue(ctx context.Context) {
	free := cap(s.sem) - len(s.sem)
	if free == 0 {
		return
	}
	due, err := s.o.Store.ClaimDue(ctx, free, s.lease())
	if err != nil {
		if ctx.Err() == nil {
			s.o.Logger.ErrorContext(ctx, "could not claim due website checks", slog.String("error", err.Error()))
		}
		return
	}
	for _, d := range due {
		s.sem <- struct{}{}
		s.wg.Add(1)
		go func() {
			defer func() { <-s.sem; s.wg.Done() }()
			s.check(ctx, d)
		}()
	}
}

// Wait blocks until every started check has been recorded.
func (s *Scheduler) Wait() { s.wg.Wait() }

// lease outlasts the longest possible check, so a crashed server's claim
// is retried rather than lost.
func (s *Scheduler) lease() time.Duration {
	return time.Duration(MaxTimeoutSeconds)*time.Second + time.Minute
}

func (s *Scheduler) check(ctx context.Context, d Due) {
	m := d.Monitor
	log := s.o.Logger.With(slog.String("monitor_id", m.ID), slog.String("host", Host(m.URL)))
	if d.SecretUnreadable {
		log.ErrorContext(ctx, "website check skipped: the monitor's secret cannot be decrypted with the configured key")
		if err := s.o.Store.Reschedule(ctx, m.ID, s.o.Now().Add(time.Duration(m.IntervalSeconds)*time.Second)); err != nil {
			log.ErrorContext(ctx, "could not reschedule a website check", slog.String("error", err.Error()))
		}
		return
	}

	r := s.o.Checker.Check(ctx, m, d.Secret)
	if ctx.Err() != nil {
		return // shutting down: the lease expires and the check reruns
	}
	now := s.o.Now()
	offline, changed := s.o.Outage.Observe(ctx, Host(m.URL), r, now)
	if changed {
		if offline {
			log.WarnContext(ctx, "website probe appears to have no outbound connectivity; failures are recorded as unknown and no monitor is marked down")
		} else {
			log.InfoContext(ctx, "website probe connectivity restored")
		}
	}

	next, beat, event := Apply(m, d.Counters, r, offline, now)
	if event != nil {
		event.MonitorID = m.ID
	}
	rec := CheckRecord{
		MonitorID: m.ID, At: now, Counters: next, Heartbeat: beat, Result: r, Event: event,
		NextCheckAt: now.Add(NextInterval(m, next)), CertExpiresAt: r.CertExpiresAt,
	}
	if err := s.o.Store.RecordCheck(ctx, rec); err != nil {
		log.ErrorContext(ctx, "could not record a website check", slog.String("error", err.Error()))
		return
	}
	level := slog.LevelDebug
	if event != nil {
		level = slog.LevelInfo
	}
	log.Log(ctx, level, "website check", slog.String("status", string(next.Status)),
		slog.String("failure", string(r.Failure)), slog.Int("status_code", r.StatusCode))
}

func (s *Scheduler) prune(ctx context.Context) {
	n, err := s.o.Store.PruneEvents(ctx, s.o.Now().Add(-s.o.EventRetention))
	if err != nil {
		if ctx.Err() == nil {
			s.o.Logger.ErrorContext(ctx, "could not prune website events", slog.String("error", err.Error()))
		}
		return
	}
	if n > 0 {
		s.o.Logger.InfoContext(ctx, "pruned website events", slog.Int64("events", n))
	}
}
