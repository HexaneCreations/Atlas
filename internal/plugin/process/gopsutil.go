package process

import (
	"context"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	gops "github.com/shirou/gopsutil/v4/process"
)

// maxCmdline bounds the stored command line.
//
// A truncated command line still identifies the process; an untruncated one
// can be many kilobytes for a JVM, and multiplying that by a thousand
// processes turns an inventory response into megabytes.
const maxCmdline = 512

type gopsutilProvider struct{}

// NewProvider returns a Provider backed by gopsutil.
func NewProvider() Provider { return gopsutilProvider{} }

func (gopsutilProvider) Processes(ctx context.Context) ([]Process, error) {
	const op = "process.Provider.Processes"

	procs, err := gops.ProcessesWithContext(ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not enumerate processes").WithOp(op)
	}

	out := make([]Process, 0, len(procs))
	for _, p := range procs {
		// Honour cancellation between processes. A sweep of two thousand
		// processes is thousands of file reads, and a wedged /proc entry
		// must not consume the whole run budget.
		if ctx.Err() != nil {
			break
		}

		proc, ok := readProcess(ctx, p)
		if !ok {
			continue
		}
		out = append(out, proc)
	}
	return out, nil
}

// readProcess assembles one process, tolerating fields the caller may not be
// permitted to read.
//
// Every field is best-effort by design. Atlas frequently runs unprivileged, so
// another user's command line and executable path are simply not readable —
// and a process that exits mid-sweep makes every subsequent read fail. Neither
// is an error worth losing the process over: a PID with a name and a state is
// still worth counting.
func readProcess(ctx context.Context, p *gops.Process) (Process, bool) {
	name, err := p.NameWithContext(ctx)
	if err != nil {
		// No name means the process is gone. Nothing useful remains.
		return Process{}, false
	}

	proc := Process{PID: p.Pid, Name: name, State: StateOther}

	if ppid, err := p.PpidWithContext(ctx); err == nil {
		proc.PPID = ppid
	}
	if exe, err := p.ExeWithContext(ctx); err == nil {
		proc.Executable = exe
	}
	if cmdline, err := p.CmdlineWithContext(ctx); err == nil {
		if len(cmdline) > maxCmdline {
			cmdline = cmdline[:maxCmdline] + "…"
		}
		proc.Cmdline = cmdline
	}
	if username, err := p.UsernameWithContext(ctx); err == nil {
		proc.Username = username
	}
	if states, err := p.StatusWithContext(ctx); err == nil && len(states) > 0 {
		proc.State = normaliseState(states[0])
	}
	if cpu, err := p.CPUPercentWithContext(ctx); err == nil {
		proc.CPUPercent = cpu
	}
	if mem, err := p.MemoryInfoWithContext(ctx); err == nil && mem != nil {
		proc.MemoryRSS = mem.RSS
	}
	if memPct, err := p.MemoryPercentWithContext(ctx); err == nil {
		proc.MemoryPercent = float64(memPct)
	}
	if threads, err := p.NumThreadsWithContext(ctx); err == nil {
		proc.Threads = threads
	}
	if created, err := p.CreateTimeWithContext(ctx); err == nil && created > 0 {
		proc.CreatedAt = time.UnixMilli(created)
	}

	return proc, true
}

// normaliseState maps platform-specific status letters onto Atlas's set.
//
// Linux reports single letters from /proc, macOS reports its own set, and
// gopsutil passes both through. Normalising here means the metric labels are
// identical across a mixed fleet, which is what makes a fleet-wide query
// possible at all.
func normaliseState(status string) State {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "R", "RUNNING":
		return StateRunning
	case "S", "D", "SLEEP", "SLEEPING", "BLOCKED", "IDLE_SLEEP":
		return StateSleeping
	case "T", "STOP", "STOPPED":
		return StateStopped
	case "Z", "ZOMBIE":
		return StateZombie
	case "I", "IDLE":
		return StateIdle
	default:
		return StateOther
	}
}
