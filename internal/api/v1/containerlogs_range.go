package v1

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/plugin/docker"
)

// Limits on one ranged log response. The scan limits are configuration
// (config.ContainerLogs); these bound what a single response may carry.
const (
	defaultLogLimit = 500
	maxLogLimit     = 2000
	// maxLogResponseBytes bounds the messages in one response.
	maxLogResponseBytes = 4 << 20
	// maxLogLineBytes cuts one very long line; the line says it was cut.
	maxLogLineBytes = 16 << 10
	// firstLogWindow is the first window a page searches back from its end.
	// Each window that holds fewer lines than the page needs is followed by
	// one four times longer, so a page costs at most about 4/3 of the lines
	// in the window that finally fills it, wherever the page is in the
	// range, rather than a rescan from the start of the range.
	firstLogWindow  = 5 * time.Minute
	logWindowGrowth = 4
)

// logReader reads one window of a container's logs, oldest first. until is
// a request, not a guarantee: a version 1 Agent cannot honour it, and the
// caller always stops at it itself.
type logReader func(ctx context.Context, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error)

// rangeSource is one container's log source for a ranged read.
type rangeSource struct {
	read logReader
	// direct is false when the source cannot end a read at Until (a version
	// 1 Agent): every window then reads on to its first later line.
	direct bool
	// onUnsupported is told when the source turns out not to support Until
	// after all; the read is then redone without it.
	onUnsupported func()
}

// rangePage is one page of a ranged read, oldest line first.
type rangePage struct {
	Lines []docker.LogLine
	// More is true when older lines exist inside the range; NextBefore is
	// then the cursor for them.
	More       bool
	NextBefore *time.Time
	// ScanLimited is true when reading stopped at a scan limit or the
	// deadline before the page could be completed.
	ScanLimited bool
	// Truncated is true when the response cap dropped older lines of the
	// page (More is then true as well).
	Truncated bool
	// AvailableSince is the oldest line the container still has, when that
	// is later than the start of the range.
	AvailableSince *time.Time
	// Fallback is true when the source could not end reads at a time and
	// the server bounded them itself.
	Fallback bool
}

// rangeQuery is a validated ranged read: lines in [From, Upper], newest
// Limit of them. Upper zero means now and open-ended.
type rangeQuery struct {
	From  time.Time
	Upper time.Time
	Limit int
	// Until is the range's end as asked, clamped to now; zero for a range
	// that ends now.
	Until time.Time
}

type windowResult struct {
	lines   []docker.LogLine // the newest keep lines read, oldest first
	read    int              // lines inside the window
	limited bool
	err     error
}

// readWindow reads [start, end] and keeps the newest keep lines of it.
func readWindow(parent context.Context, src *rangeSource, start, end time.Time, keep int, budget *scanBudget) windowResult {
	ctx, cancel := context.WithCancel(parent)
	defer cancel() // also ends a version 1 Agent's read at its first line past end
	opts := docker.LogOptions{Since: start}
	if src.direct {
		opts.Until = end
	}
	lines, errCh := src.read(ctx, opts)
	ring := make([]docker.LogLine, 0, keep)
	var res windowResult
	stopped := false
	for line := range lines {
		if line.Time.After(end) {
			stopped = true
			break
		}
		if line.Time.Before(start) {
			continue
		}
		if !budget.spend(len(line.Message)) {
			res.limited, stopped = true, true
			break
		}
		res.read++
		if len(ring) == keep {
			copy(ring, ring[1:])
			ring = ring[:keep-1]
		}
		ring = append(ring, line)
	}
	cancel()
	drain(lines)
	err := <-errCh
	switch {
	case parent.Err() != nil:
		res.limited = true // the request's deadline
	case err != nil && !stopped:
		res.err = err
	}
	res.lines = ring
	return res
}

// drain empties a log channel whose read was cancelled, so its producer can
// finish.
func drain(lines <-chan docker.LogLine) {
	for range lines {
		continue
	}
}

// scanBudget is one request's shared scan allowance.
type scanBudget struct {
	lines, bytes int64
	deadline     time.Time
}

func newScanBudget(cfg config.ContainerLogs, now time.Time) *scanBudget {
	return &scanBudget{lines: int64(cfg.ScanMaxLines), bytes: cfg.ScanMaxBytes, deadline: now.Add(cfg.ScanTimeout)}
}

func (b *scanBudget) spend(n int) bool {
	b.lines--
	b.bytes -= int64(n)
	return b.lines >= 0 && b.bytes >= 0 && !b.expired()
}

func (b *scanBudget) expired() bool { return !time.Now().Before(b.deadline) }

// readRange answers q from src. An open-ended first page asks for the tail
// directly, which every source serves; a bounded one searches back from its
// end in growing windows, because Docker applies a tail before since and
// until and so cannot return "the newest N lines before a time".
func readRange(ctx context.Context, src *rangeSource, q rangeQuery, budget *scanBudget) (rangePage, error) {
	page := rangePage{Fallback: !src.direct}
	keep := q.Limit + 1 // one more than the page says whether older lines exist
	var lines []docker.LogLine
	reachedFrom := false

	if q.Upper.IsZero() {
		ch, errCh := src.read(ctx, docker.LogOptions{Since: q.From, Tail: keep})
		for line := range ch {
			if !line.Time.Before(q.From) {
				lines = append(lines, line)
			}
		}
		if err := <-errCh; err != nil && len(lines) == 0 {
			return page, err
		}
		reachedFrom = len(lines) < keep
	} else {
		for w := firstLogWindow; ; w *= logWindowGrowth {
			start := q.Upper.Add(-w)
			if !start.After(q.From) {
				start = q.From
			}
			res := readWindow(ctx, src, start, q.Upper, keep, budget)
			if res.err != nil && src.direct && errors.Is(res.err, docker.ErrUntilUnsupported) {
				src.direct, page.Fallback = false, true
				if src.onUnsupported != nil {
					src.onUnsupported()
				}
				res = readWindow(ctx, src, start, q.Upper, keep, budget)
			}
			if res.err != nil && len(res.lines) == 0 {
				return page, res.err
			}
			lines = res.lines
			if res.limited {
				page.ScanLimited = true
				break
			}
			if res.read >= keep {
				break
			}
			if start.Equal(q.From) {
				reachedFrom = true
				break
			}
		}
	}

	if len(lines) > q.Limit {
		lines = lines[len(lines)-q.Limit:]
		page.More = true
	}
	lines, page.Truncated = capPage(lines)
	if page.Truncated {
		page.More = true
	}
	page.Lines = lines
	if page.More && !page.ScanLimited && len(lines) > 0 {
		next := lines[0].Time
		page.NextBefore = &next
	}
	if reachedFrom && !page.More && !page.ScanLimited {
		page.AvailableSince = availableSince(ctx, src, q.From, lines)
	}
	return page, nil
}

// capPage keeps the newest lines that fit maxLogResponseBytes and cuts any
// single line longer than maxLogLineBytes.
func capPage(lines []docker.LogLine) ([]docker.LogLine, bool) {
	total, first := 0, len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		n := min(len(lines[i].Message), maxLogLineBytes)
		if total+n > maxLogResponseBytes {
			break
		}
		total += n
		first = i
	}
	return lines[first:], first > 0
}

// availableSince is the container's oldest line, when it is later than
// from: the range starts before what Docker still keeps. One read of the
// very first line answers it, for any source.
func availableSince(ctx context.Context, src *rangeSource, from time.Time, page []docker.LogLine) *time.Time {
	if len(page) > 0 && page[0].Time.Sub(from) < time.Minute {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, errCh := src.read(ctx, docker.LogOptions{})
	line, ok := <-ch
	cancel()
	drain(ch)
	<-errCh
	if !ok || !line.Time.After(from) {
		return nil
	}
	t := line.Time
	return &t
}

// scanLimiter bounds concurrent bounded reads per node and per user.
type scanLimiter struct {
	mu               sync.Mutex
	perNode, perUser int
	nodes, users     map[string]int
}

func newScanLimiter(perNode, perUser int) *scanLimiter {
	return &scanLimiter{perNode: perNode, perUser: perUser, nodes: map[string]int{}, users: map[string]int{}}
}

// acquire reserves a scan for nodeID and userID; release undoes it.
func (l *scanLimiter) acquire(nodeID, userID string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nodes[nodeID] >= l.perNode || l.users[userID] >= l.perUser {
		return nil, false
	}
	l.nodes[nodeID]++
	l.users[userID]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.nodes[nodeID]--
		l.users[userID]--
	}, true
}
