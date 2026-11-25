package app

import (
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/log"
)

func TestRegisteredIntervalsComeFromTheCollectors(t *testing.T) {
	// A default no collector uses, so a collector that stopped declaring its
	// own interval would show up here.
	got, err := registeredIntervals(t.Context(), 7*time.Second, log.Discard())
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]time.Duration{
		containerCollectorID: 30 * time.Second,
		serviceCollectorID:   60 * time.Second,
		"docker.stats":       7 * time.Second, // declares none
	} {
		if got[id] != want {
			t.Errorf("%s interval = %v, want %v", id, got[id], want)
		}
	}
}
