package notification

import (
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// MutedIndefinitely is the alerts_muted_until that means "until unmuted".
// It is an ordinary future time to the evaluator and to EndMuteAfterExpiry,
// so neither ever ends such a mute. A client recognises it by equality, or
// by a year of 9999. It is not the very last second of that year because
// encoding/json refuses a time whose local year is 10000.
var MutedIndefinitely = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// muteDurations are the only mute lengths accepted.
var muteDurations = map[string]time.Duration{
	"1h": time.Hour, "4h": 4 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
}

// MuteUntil resolves a mute duration name to when the mute ends.
func MuteUntil(duration string, now time.Time) (time.Time, error) {
	if duration == "until_unmuted" {
		return MutedIndefinitely, nil
	}
	d, ok := muteDurations[duration]
	if !ok {
		return time.Time{}, errs.New(errs.CodeInvalidArgument, "duration must be 1h, 4h, 24h, 7d or until_unmuted").
			WithOp("notification.MuteUntil").WithDetail("field", "duration")
	}
	return now.Add(d).UTC().Truncate(time.Microsecond), nil
}

// NodeAlerts is a node's mute and archive columns.
type NodeAlerts struct {
	MutedUntil *time.Time
	ArchivedAt *time.Time
}
