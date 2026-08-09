// Package id generates identifiers for requests, events, and samples.
//
// Atlas mints identifiers on hot paths — one per event, one per request — so
// the generator must be cheap. A UUIDv4 costs a call into the OS entropy pool
// per identifier; instead this package draws randomness once at process start
// and appends a monotonic counter.
//
// The result is:
//
//   - Unique within a process by construction, because the counter never
//     repeats.
//   - Unique across processes with overwhelming probability, because the
//     64-bit random prefix differs.
//   - Lexicographically ordered within a process, which makes identifiers
//     sort by creation order in logs and database indexes.
//
// These identifiers are not secrets and must never be used as capabilities:
// the counter component is trivially predictable. Session tokens and API keys
// use crypto/rand directly.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync/atomic"
)

// Length is the number of characters in a generated identifier.
const Length = 32

// prefix is drawn once per process from the OS entropy pool.
var prefix = func() string {
	var b [8]byte
	// rand.Read is documented never to fail as of Go 1.24; it panics on a
	// broken entropy source rather than returning an error.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}()

var counter atomic.Uint64

// New returns a new identifier.
//
// It is safe for concurrent use and performs no allocation beyond the
// returned string.
func New() string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], counter.Add(1))
	return prefix + hex.EncodeToString(b[:])
}
