package healthscore

import (
	"context"
	"fmt"

	"github.com/hexane/atlas/internal/core/incident"
)

// IncidentReader is the incident read path. Satisfied directly by
// [github.com/hexane/atlas/internal/core/incident.Store] and by the app
// layer's existing lazy incident store — see internal/app.
type IncidentReader interface {
	ListIncidents(ctx context.Context, filter incident.Filter) ([]incident.Incident, error)
}

// IncidentProvider scores a node on its open incidents: no open incidents
// scores 100, and each open incident touching the node lowers the score by
// its severity.
type IncidentProvider struct {
	Store IncidentReader
}

func (p IncidentProvider) Name() string { return "incidents" }

func (p IncidentProvider) Score(ctx context.Context, nodeID string) (Signal, error) {
	incidents, err := p.Store.ListIncidents(ctx, incident.Filter{Status: incident.StatusOpen, NodeID: nodeID})
	if err != nil {
		return Signal{}, err
	}

	var critical, warning int
	for _, inc := range incidents {
		if inc.Severity == incident.SeverityCritical {
			critical++
		} else {
			warning++
		}
	}

	score := max(100.0-float64(critical)*50-float64(warning)*20, 0)
	detail := "no open incidents"
	if critical > 0 || warning > 0 {
		detail = fmt.Sprintf("%d critical, %d warning open", critical, warning)
	}
	return Signal{Score: score, Available: true, Detail: detail}, nil
}
