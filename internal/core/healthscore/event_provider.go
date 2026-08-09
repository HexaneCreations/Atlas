package healthscore

import (
	"context"
	"fmt"
	"time"

	"github.com/hexane/atlas/internal/core/activity"
	"github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

const eventWindow = 15 * time.Minute

// EventReader queries durable events. Satisfied directly by
// [*github.com/hexane/atlas/internal/storage/eventstore.Repository].
type EventReader interface {
	Query(ctx context.Context, filter eventstore.Filter) ([]eventstore.Record, error)
}

// EventProvider scores a node on how many notable events (see
// [activity.Classify]) it has produced in the recent window — a burst of
// warning/danger-level events degrades this signal before any alert rule or
// incident has caught up to it.
type EventProvider struct {
	Events EventReader
}

func (p EventProvider) Name() string { return "events" }

func (p EventProvider) Score(ctx context.Context, nodeID string) (Signal, error) {
	records, err := p.Events.Query(ctx, eventstore.Filter{NodeID: nodeID, Since: time.Now().Add(-eventWindow)})
	if err != nil {
		return Signal{}, err
	}

	var danger, warning int
	for _, rec := range records {
		sev, ok := activity.Classify(eventbus.Topic(rec.Topic))
		if !ok {
			continue
		}
		switch sev {
		case activity.SeverityDanger:
			danger++
		case activity.SeverityWarning:
			warning++
		}
	}

	score := max(100.0-float64(danger)*10-float64(warning)*4, 0)
	detail := "no notable events"
	if danger > 0 || warning > 0 {
		detail = fmt.Sprintf("%d danger, %d warning events in the last %s", danger, warning, eventWindow)
	}
	return Signal{Score: score, Available: true, Detail: detail}, nil
}
