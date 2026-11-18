//go:build integration

package metric

// UpsertLatest exposes upsertLatest to the external integration tests, which
// need to drive it inside their own transactions.
var UpsertLatest = upsertLatest
