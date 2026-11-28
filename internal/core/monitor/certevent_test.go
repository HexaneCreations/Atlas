package monitor

import (
	"testing"
	"time"
)

func TestCertEventMarksEachThresholdCrossingOnce(t *testing.T) {
	t.Parallel()
	m := Defaults() // warn 14 days, critical 3
	day := 24 * time.Hour
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	prev := now.Add(-time.Minute)
	for _, c := range []struct {
		name          string
		before, after *time.Time
		want          EventKind
	}{
		{"first certificate, far off", nil, at(60 * day), ""},
		{"first certificate, already in the warning window", nil, at(10 * day), EventCertWarn},
		{"still far off", at(60 * day), at(60 * day), ""},
		{"enters the warning window", at(14*day + 30*time.Second), at(14*day - time.Second), EventCertWarn},
		{"stays in the warning window", at(10 * day), at(10 * day), ""},
		{"enters the critical window", at(3*day + 30*time.Second), at(3*day - time.Second), EventCertCrit},
		{"warning straight to expired", at(10 * day), at(-time.Hour), EventCertCrit},
		{"renewed from critical", at(day), at(90 * day), EventCertRenewed},
		{"renewed from warning", at(10 * day), at(90 * day), EventCertRenewed},
		{"renewed, but still inside the warning window", at(day), at(10 * day), ""},
		{"no certificate seen this check", at(day), nil, ""},
	} {
		got := CertEvent(m, Counters{LastCheckAt: &prev, CertExpiresAt: c.before}, c.after, now)
		switch {
		case c.want == "" && got != nil:
			t.Errorf("%s: event %s, want none", c.name, got.Kind)
		case c.want != "" && (got == nil || got.Kind != c.want || !got.CertExpiresAt.Equal(*c.after)):
			t.Errorf("%s: event %+v, want %s", c.name, got, c.want)
		}
	}

	m.CertNotify = false
	if got := CertEvent(m, Counters{}, at(day), now); got != nil {
		t.Errorf("cert_notify off recorded %+v", got)
	}
}
