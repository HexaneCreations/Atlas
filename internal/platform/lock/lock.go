// Package lock provides an OS-level exclusive lock used to guarantee that at
// most one process operates on a given directory at a time.
//
// It exists so the Agent can refuse to start a second instance against a
// data directory that's already in use — protecting identity, certificates,
// spool, and relationship state from concurrent, uncoordinated writers. The
// lock is acquired via flock(2), which is atomic at the kernel level and
// tied to the open file description rather than a PID, so it is race-safe
// (no pgrep-then-start TOCTOU window) and releases automatically on process
// exit or crash, including SIGKILL/OOM-kill, with no cleanup code required.
package lock

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrHeld is returned by Acquire when another process already holds the
// lock. PID is diagnostic only — exclusivity comes from flock, not from it.
type ErrHeld struct {
	Path string
	PID  int
}

func (e *ErrHeld) Error() string {
	return fmt.Sprintf("another agent instance is already running\npid: %d\ndata_dir: %s", e.PID, e.Path)
}

// Lock is a held exclusive lock. Obtain one via Acquire.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive, non-blocking lock on path, creating it if
// necessary. The returned Lock must be kept open for as long as exclusivity
// is required. If another process holds the lock, Acquire returns *ErrHeld
// immediately rather than blocking.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readHolderPID(f)
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, &ErrHeld{Path: path, PID: holder}
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}

	if err := stampHolder(f); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, fmt.Errorf("write lock holder: %w", err)
	}

	return &Lock{f: f}, nil
}

// Release unlocks and closes the underlying file. The lock file is
// deliberately left on disk: unlinking it would reopen the TOCTOU race
// flock(2) exists to close, because a second process could create and lock
// a fresh inode between the unlink and this process's exit.
func (l *Lock) Release() error {
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}

func stampHolder(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	_, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n" + time.Now().UTC().Format(time.RFC3339) + "\n")
	if err != nil {
		return err
	}
	return f.Sync()
}

// readHolderPID yields 0 for a malformed or unstamped file: the conflict is
// real regardless of whether the holder's PID could be read.
func readHolderPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	line, _, _ := strings.Cut(string(buf[:n]), "\n")
	pid, _ := strconv.Atoi(strings.TrimSpace(line))
	return pid
}
