package v1

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/plugin/docker"
)

// fakeLogs behaves like Docker: a tail is taken from the whole log first,
// then since and until filter it. honoursUntil false is a version 1 Agent,
// which streams past until; refusesUntil is one the server has not yet
// learned is version 1.
type fakeLogs struct {
	mu           sync.Mutex
	lines        []docker.LogLine
	honoursUntil bool
	refusesUntil bool
	calls        []docker.LogOptions
	streamed     int
}

func (f *fakeLogs) read(ctx context.Context, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
	f.mu.Lock()
	f.calls = append(f.calls, opts)
	f.mu.Unlock()
	out, errCh := make(chan docker.LogLine), make(chan error, 1)
	go func() {
		defer close(errCh)
		defer close(out)
		if f.refusesUntil && !opts.Until.IsZero() {
			errCh <- fmt.Errorf("agent: %w", docker.ErrUntilUnsupported)
			return
		}
		src := f.lines
		if opts.Tail > 0 && opts.Tail < len(src) {
			src = src[len(src)-opts.Tail:]
		}
		for _, l := range src {
			if !opts.Since.IsZero() && l.Time.Before(opts.Since) {
				continue
			}
			if f.honoursUntil && !opts.Until.IsZero() && l.Time.After(opts.Until) {
				return
			}
			select {
			case out <- l:
				f.mu.Lock()
				f.streamed++
				f.mu.Unlock()
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
	}()
	return out, errCh
}

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// everyMinute is one line per minute for n minutes from t0.
func everyMinute(n int) []docker.LogLine {
	out := make([]docker.LogLine, n)
	for i := range out {
		out[i] = docker.LogLine{Time: t0.Add(time.Duration(i) * time.Minute), Stream: docker.StreamStdout, Message: fmt.Sprintf("line %d", i)}
	}
	return out
}

func budget() *scanBudget {
	return newScanBudget(config.Default().ContainerLogs, time.Now())
}

func messages(lines []docker.LogLine) string {
	var b []string
	for _, l := range lines {
		b = append(b, strings.TrimPrefix(l.Message, "line "))
	}
	return strings.Join(b, ",")
}

func TestReadRangeReturnsTheNewestLinesBeforeUntilOnEveryPath(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		src  *fakeLogs
	}{
		{"agent honours until", &fakeLogs{lines: everyMinute(600), honoursUntil: true}},
		{"version 1 agent", &fakeLogs{lines: everyMinute(600)}},
	} {
		src := &rangeSource{read: c.src.read, direct: c.src.honoursUntil}
		q := rangeQuery{From: t0, Upper: t0.Add(100 * time.Minute), Limit: 5}
		page, err := readRange(context.Background(), src, q, budget())
		if err != nil {
			t.Fatal(err)
		}
		if got := messages(page.Lines); got != "96,97,98,99,100" || !page.More || page.NextBefore == nil ||
			!page.NextBefore.Equal(t0.Add(96*time.Minute)) || page.Fallback == c.src.honoursUntil {
			t.Errorf("%s: page %s more=%v next=%v fallback=%v", c.name, got, page.More, page.NextBefore, page.Fallback)
		}
		for _, call := range c.src.calls {
			if call.Tail != 0 {
				t.Errorf("%s: a bounded read used a tail (%+v); Docker would return nothing", c.name, call)
			}
		}
		// The next page starts from the cursor, not from the start of the range.
		q.Upper = page.NextBefore.Add(-time.Nanosecond)
		c.src.calls = nil
		page, err = readRange(context.Background(), src, q, budget())
		if err != nil || messages(page.Lines) != "91,92,93,94,95" {
			t.Errorf("%s: second page %s, %v", c.name, messages(page.Lines), err)
		}
		for _, call := range c.src.calls {
			if call.Since.Before(t0.Add(60 * time.Minute)) {
				t.Errorf("%s: an older page rescanned from %v", c.name, call.Since)
			}
		}
	}
}

func TestReadRangeVersionOneAgentStopsAtTheFirstLineAfterUntil(t *testing.T) {
	t.Parallel()
	f := &fakeLogs{lines: everyMinute(10_000)}
	src := &rangeSource{read: f.read}
	if _, err := readRange(context.Background(), src, rangeQuery{From: t0, Upper: t0.Add(30 * time.Minute), Limit: 5}, budget()); err != nil {
		t.Fatal(err)
	}
	if f.streamed > 50 {
		t.Errorf("streamed %d lines for a 30-minute window; the read was not cancelled at until", f.streamed)
	}
}

func TestReadRangeFallsBackWhenTheAgentRefusesUntil(t *testing.T) {
	t.Parallel()
	f := &fakeLogs{lines: everyMinute(200), refusesUntil: true}
	told := false
	src := &rangeSource{read: f.read, direct: true, onUnsupported: func() { told = true }}
	page, err := readRange(context.Background(), src, rangeQuery{From: t0, Upper: t0.Add(50 * time.Minute), Limit: 3}, budget())
	if err != nil || messages(page.Lines) != "48,49,50" || !page.Fallback || !told {
		t.Errorf("page %s fallback=%v told=%v err=%v", messages(page.Lines), page.Fallback, told, err)
	}
}

func TestReadRangeOpenEndedUsesATail(t *testing.T) {
	t.Parallel()
	f := &fakeLogs{lines: everyMinute(100)}
	page, err := readRange(context.Background(), &rangeSource{read: f.read}, rangeQuery{From: t0.Add(10 * time.Minute), Limit: 4}, budget())
	if err != nil || messages(page.Lines) != "96,97,98,99" || !page.More || len(f.calls) != 1 || f.calls[0].Tail != 5 {
		t.Errorf("page %s more=%v calls=%+v err=%v", messages(page.Lines), page.More, f.calls, err)
	}
	// A tail that reaches the start of the range has nothing older.
	page, _ = readRange(context.Background(), &rangeSource{read: f.read}, rangeQuery{From: t0.Add(97 * time.Minute), Limit: 10}, budget())
	if page.More || messages(page.Lines) != "97,98,99" || page.AvailableSince != nil {
		t.Errorf("short tail = %s more=%v since=%v", messages(page.Lines), page.More, page.AvailableSince)
	}
}

func TestReadRangeSaysWhenTheRangeStartsBeforeTheOldestLine(t *testing.T) {
	t.Parallel()
	f := &fakeLogs{lines: everyMinute(30), honoursUntil: true} // the log starts at t0
	from := t0.Add(-6 * time.Hour)
	page, err := readRange(context.Background(), &rangeSource{read: f.read, direct: true},
		rangeQuery{From: from, Upper: t0.Add(10 * time.Minute), Limit: 100}, budget())
	if err != nil || page.More || page.AvailableSince == nil || !page.AvailableSince.Equal(t0) || len(page.Lines) != 11 {
		t.Errorf("page %d lines, more=%v available=%v err=%v", len(page.Lines), page.More, page.AvailableSince, err)
	}
	// The whole range before the oldest line: empty, but explained.
	page, _ = readRange(context.Background(), &rangeSource{read: f.read, direct: true},
		rangeQuery{From: from, Upper: t0.Add(-time.Hour), Limit: 100}, budget())
	if len(page.Lines) != 0 || page.AvailableSince == nil || !page.AvailableSince.Equal(t0) {
		t.Errorf("empty range: %d lines, available=%v", len(page.Lines), page.AvailableSince)
	}
	// A container with no logs at all says nothing it does not know.
	empty := &fakeLogs{honoursUntil: true}
	page, _ = readRange(context.Background(), &rangeSource{read: empty.read, direct: true},
		rangeQuery{From: from, Upper: t0, Limit: 100}, budget())
	if len(page.Lines) != 0 || page.AvailableSince != nil {
		t.Errorf("no logs: %+v", page)
	}
}

func TestReadRangeStopsAtTheScanLimit(t *testing.T) {
	t.Parallel()
	lines := make([]docker.LogLine, 5000) // 5000 lines inside one second
	for i := range lines {
		lines[i] = docker.LogLine{Time: t0.Add(time.Duration(i) * time.Microsecond), Message: "x"}
	}
	f := &fakeLogs{lines: lines, honoursUntil: true}
	cfg := config.Default().ContainerLogs
	cfg.ScanMaxLines = 1000
	page, err := readRange(context.Background(), &rangeSource{read: f.read, direct: true},
		rangeQuery{From: t0.Add(-time.Hour), Upper: t0.Add(time.Second), Limit: 2000}, newScanBudget(cfg, time.Now()))
	if err != nil || !page.ScanLimited || page.NextBefore != nil || page.AvailableSince != nil {
		t.Errorf("scan limit: limited=%v next=%v err=%v", page.ScanLimited, page.NextBefore, err)
	}
	cfg = config.Default().ContainerLogs
	cfg.ScanTimeout = time.Nanosecond
	page, _ = readRange(context.Background(), &rangeSource{read: f.read, direct: true},
		rangeQuery{From: t0.Add(-time.Hour), Upper: t0.Add(time.Second), Limit: 10}, newScanBudget(cfg, time.Now().Add(-time.Second)))
	if !page.ScanLimited {
		t.Error("an expired deadline did not mark the page limited")
	}
}

func TestReadRangeCapsTheResponseSize(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("y", maxLogLineBytes*2)
	lines := make([]docker.LogLine, 300)
	for i := range lines {
		lines[i] = docker.LogLine{Time: t0.Add(time.Duration(i) * time.Second), Message: big}
	}
	f := &fakeLogs{lines: lines, honoursUntil: true}
	page, err := readRange(context.Background(), &rangeSource{read: f.read, direct: true},
		rangeQuery{From: t0, Upper: t0.Add(time.Hour), Limit: 300}, budget())
	if err != nil || !page.Truncated || !page.More || len(page.Lines) != maxLogResponseBytes/maxLogLineBytes {
		t.Errorf("lines %d truncated=%v more=%v err=%v", len(page.Lines), page.Truncated, page.More, err)
	}
	if !page.Lines[len(page.Lines)-1].Time.Equal(lines[299].Time) {
		t.Error("the cap dropped the newest lines instead of the oldest")
	}
}

func TestReadRangeSurfacesASourceError(t *testing.T) {
	t.Parallel()
	failing := func(context.Context, docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
		out, errCh := make(chan docker.LogLine), make(chan error, 1)
		close(out)
		errCh <- errors.New("no active libp2p session")
		close(errCh)
		return out, errCh
	}
	if _, err := readRange(context.Background(), &rangeSource{read: failing, direct: true},
		rangeQuery{From: t0, Upper: t0.Add(time.Hour), Limit: 5}, budget()); err == nil {
		t.Error("a failing source returned an empty page")
	}
}

func TestParseLogRange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for _, c := range []struct {
		query, field string
	}{
		{"until=" + ts(-time.Hour), "since"},
		{"since=yesterday", "since"},
		{"since=2026-03-02 10:00:00", "since"},
		{"since=" + ts(-time.Hour) + "&until=" + ts(-2*time.Hour), "until"},
		{"since=" + ts(-time.Hour) + "&until=" + ts(-time.Hour), "until"},
		{"since=" + ts(time.Hour), "since"},
		{"since=" + ts(-2*time.Hour) + "&until=" + ts(time.Hour), "until"},
		{"since=" + ts(-25*time.Hour), "since"},
		{"since=" + ts(-30*time.Hour) + "&until=" + ts(-5*time.Hour), "since"},
		{"since=" + ts(-time.Hour) + "&limit=0", "limit"},
		{"since=" + ts(-time.Hour) + "&limit=2001", "limit"},
		{"since=" + ts(-time.Hour) + "&limit=ten", "limit"},
		{"since=" + ts(-time.Hour) + "&limit=", "limit"},
		{"since=" + ts(-time.Hour) + "&tail=10&limit=5", "tail"},
		{"since=" + ts(-time.Hour) + "&before=" + ts(-2*time.Hour), "before"},
		{"since=" + ts(-time.Hour) + "&until=" + ts(-30*time.Minute) + "&before=" + ts(-10*time.Minute), "before"},
		{"since=" + ts(-time.Hour) + "&until=", "until"},
	} {
		q, _ := url.ParseQuery(c.query)
		_, err := parseLogRange(q, 24*time.Hour, now)
		if err == nil || errs.CodeOf(err) != errs.CodeInvalidArgument || errs.Details(err)["field"] != c.field {
			t.Errorf("%s: err %v, want a rejection naming %s", c.query, err, c.field)
		}
	}

	q, _ := url.ParseQuery("since=" + ts(-2*time.Hour) + "&until=" + ts(-time.Hour) + "&limit=50")
	got, err := parseLogRange(q, 24*time.Hour, now)
	if err != nil || !got.From.Equal(now.Add(-2*time.Hour)) || !got.Upper.Equal(now.Add(-time.Hour)) || got.Limit != 50 {
		t.Errorf("valid range = %+v, %v", got, err)
	}
	q, _ = url.ParseQuery("since=" + ts(-2*time.Hour))
	if got, err = parseLogRange(q, 24*time.Hour, now); err != nil || !got.Upper.IsZero() || got.Limit != defaultLogLimit {
		t.Errorf("open-ended = %+v, %v", got, err)
	}
	q, _ = url.ParseQuery("since=" + ts(-2*time.Hour) + "&before=" + now.Add(-time.Hour).Format(time.RFC3339Nano))
	if got, err = parseLogRange(q, 24*time.Hour, now); err != nil || !got.Upper.Equal(now.Add(-time.Hour-time.Nanosecond)) {
		t.Errorf("cursor = %+v, %v", got, err)
	}
	// A few seconds of clock skew still means now.
	q, _ = url.ParseQuery("since=" + ts(-time.Hour) + "&until=" + ts(20*time.Second))
	if got, err = parseLogRange(q, 24*time.Hour, now); err != nil || !got.Upper.Equal(now) {
		t.Errorf("skewed until = %+v, %v", got, err)
	}
}

func TestScanLimiterBoundsPerNodeAndPerUser(t *testing.T) {
	t.Parallel()
	l := newScanLimiter(1, 2)
	r1, ok := l.acquire("n1", "u1")
	if !ok {
		t.Fatal("first scan refused")
	}
	if _, again := l.acquire("n1", "u2"); again {
		t.Error("a second scan on one node was allowed")
	}
	r2, ok := l.acquire("n2", "u1")
	if !ok {
		t.Fatal("a scan on another node was refused")
	}
	if _, third := l.acquire("n3", "u1"); third {
		t.Error("a third scan for one user was allowed")
	}
	r1()
	r2()
	if _, back := l.acquire("n1", "u1"); !back {
		t.Error("released slots were not returned")
	}
}

func TestCutUTF8NeverSplitsACharacter(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("é", 10) // two bytes each
	if got := cutUTF8(s, 5); got != "éé" {
		t.Errorf("cut = %q", got)
	}
	if cutUTF8("abc", 10) != "abc" {
		t.Error("a short string was cut")
	}
}
