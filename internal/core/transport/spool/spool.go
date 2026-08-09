// Package spool is the disk-backed buffer between a scheduler and a
// networked transport, closing readiness-review gap C3: without it, any
// control-plane outage permanently loses every observation made during it.
//
// Only [transport.ClassStream] envelopes belong here. A [transport.ClassSnapshot]
// envelope loses all value the moment a newer one exists, so spooling one
// during an outage just delays delivering stale data — see
// docs/architecture/agent-design.md sec 1.
package spool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
)

const (
	// DefaultMaxBytes bounds spool disk usage on a host with no explicit
	// configuration.
	DefaultMaxBytes int64 = 512 * 1024 * 1024
	// DefaultMaxAge matches the control plane's idempotency window (see
	// migrations/0004_fleet.sql, ingested_envelopes): an envelope older than
	// this can no longer be deduplicated safely if replayed, so it is
	// discarded rather than risked.
	DefaultMaxAge = 24 * time.Hour
)

const fileExt = ".envelope.json"

// Options configures a [Spool].
type Options struct {
	Dir      string
	MaxBytes int64
	MaxAge   time.Duration
	// Fsync forces each write to disk before Enqueue returns. Durability
	// against a hard crash, at the cost of disk wear and latency on
	// SSD-constrained hosts — see docs/architecture/agent-design.md sec 3.
	Fsync bool
}

func (o Options) withDefaults() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	return o
}

type entry struct {
	seq  uint64
	path string
	size int64
}

// Spool is a bounded, ordered, disk-backed queue of envelopes.
//
// Safe for concurrent use. Enqueue is expected to be called from the
// scheduler's delivery path; Peek/Dequeue from a single replay goroutine.
type Spool struct {
	dir      string
	maxBytes int64
	maxAge   time.Duration
	fsync    bool

	mu      sync.Mutex
	entries []entry
	bytes   int64
	nextSeq uint64

	dropped atomic.Uint64
}

// Open scans dir for a previously persisted spool and resumes it, or creates
// dir fresh. Entries older than MaxAge are discarded during the scan — they
// could not be safely replayed regardless.
func Open(opts Options) (*Spool, error) {
	const op = "spool.Open"
	opts = opts.withDefaults()

	if opts.Dir == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "spool requires a directory").WithOp(op)
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not create the spool directory").WithOp(op)
	}

	s := &Spool{dir: opts.Dir, maxBytes: opts.MaxBytes, maxAge: opts.MaxAge, fsync: opts.Fsync}

	files, err := os.ReadDir(opts.Dir)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not read the spool directory").WithOp(op)
	}

	cutoff := time.Now().Add(-opts.MaxAge)
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), fileExt) {
			continue
		}
		seq, ok := seqFromName(f.Name())
		if !ok {
			continue
		}
		path := filepath.Join(opts.Dir, f.Name())
		info, err := f.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
			continue
		}
		s.entries = append(s.entries, entry{seq: seq, path: path, size: info.Size()})
		s.bytes += info.Size()
		if seq >= s.nextSeq {
			s.nextSeq = seq + 1
		}
	}
	sort.Slice(s.entries, func(i, j int) bool { return s.entries[i].seq < s.entries[j].seq })

	return s, nil
}

func seqFromName(name string) (uint64, bool) {
	base := strings.TrimSuffix(name, fileExt)
	seq, err := strconv.ParseUint(base, 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// Enqueue persists env. If the spool is at [Options.MaxBytes], the oldest
// entries are dropped first — recent data is more valuable than old data
// during a recovery, and dropping it preserves continuity of what an
// operator is actively watching rather than silently losing it later anyway.
func (s *Spool) Enqueue(env transport.Envelope) error {
	const op = "spool.Spool.Enqueue"

	data, err := json.Marshal(env)
	if err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "could not encode the envelope for spooling").WithOp(op)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seq := s.nextSeq
	s.nextSeq++
	name := fmt.Sprintf("%020d%s", seq, fileExt)
	path := filepath.Join(s.dir, name)
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not open a spool file").WithOp(op)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return errs.Wrap(err, errs.CodeInternal, "could not write a spool file").WithOp(op)
	}
	if s.fsync {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return errs.Wrap(err, errs.CodeInternal, "could not sync a spool file").WithOp(op)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return errs.Wrap(err, errs.CodeInternal, "could not close a spool file").WithOp(op)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return errs.Wrap(err, errs.CodeInternal, "could not commit a spool file").WithOp(op)
	}

	s.entries = append(s.entries, entry{seq: seq, path: path, size: int64(len(data))})
	s.bytes += int64(len(data))

	for s.bytes > s.maxBytes && len(s.entries) > 1 {
		oldest := s.entries[0]
		_ = os.Remove(oldest.path)
		s.bytes -= oldest.size
		s.entries = s.entries[1:]
		s.dropped.Add(1)
	}

	return nil
}

// Peek returns the oldest queued envelope without removing it.
func (s *Spool) Peek() (transport.Envelope, bool, error) {
	const op = "spool.Spool.Peek"

	s.mu.Lock()
	if len(s.entries) == 0 {
		s.mu.Unlock()
		return transport.Envelope{}, false, nil
	}
	head := s.entries[0]
	s.mu.Unlock()

	data, err := os.ReadFile(head.path)
	if err != nil {
		return transport.Envelope{}, false, errs.Wrap(err, errs.CodeInternal, "could not read a spooled envelope").WithOp(op)
	}
	var env transport.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return transport.Envelope{}, false, errs.Wrap(err, errs.CodeInternal, "could not decode a spooled envelope").WithOp(op)
	}
	return env, true, nil
}

// PeekN returns up to n queued envelopes, oldest first, without removing
// them.
func (s *Spool) PeekN(n int) ([]transport.Envelope, error) {
	const op = "spool.Spool.PeekN"

	s.mu.Lock()
	if n > len(s.entries) {
		n = len(s.entries)
	}
	paths := make([]string, n)
	for i := range n {
		paths[i] = s.entries[i].path
	}
	s.mu.Unlock()

	out := make([]transport.Envelope, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a spooled envelope").WithOp(op)
		}
		var env transport.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not decode a spooled envelope").WithOp(op)
		}
		out = append(out, env)
	}
	return out, nil
}

// DequeueN removes the oldest n queued envelopes.
func (s *Spool) DequeueN(n int) error {
	const op = "spool.Spool.DequeueN"

	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.entries) {
		n = len(s.entries)
	}
	for i := range n {
		if err := os.Remove(s.entries[i].path); err != nil && !os.IsNotExist(err) {
			return errs.Wrap(err, errs.CodeInternal, "could not remove a spooled envelope").WithOp(op)
		}
		s.bytes -= s.entries[i].size
	}
	s.entries = s.entries[n:]
	return nil
}

// Dequeue removes the oldest queued envelope. Call it only after the
// envelope Peek returned has been durably delivered — Dequeue does not
// re-check that, since the replay loop is the only caller and it is
// single-threaded by construction.
func (s *Spool) Dequeue() error {
	const op = "spool.Spool.Dequeue"

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return errs.New(errs.CodeFailedPrecondition, "spool is empty").WithOp(op)
	}
	head := s.entries[0]
	if err := os.Remove(head.path); err != nil && !os.IsNotExist(err) {
		return errs.Wrap(err, errs.CodeInternal, "could not remove a spooled envelope").WithOp(op)
	}
	s.entries = s.entries[1:]
	s.bytes -= head.size
	return nil
}

// Len reports how many envelopes are queued.
func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Bytes reports total spooled size on disk.
func (s *Spool) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Dropped counts envelopes discarded to stay within MaxBytes.
func (s *Spool) Dropped() uint64 { return s.dropped.Load() }
