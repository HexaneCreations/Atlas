package healthscore

import (
	"context"
	"fmt"

	"github.com/hexane/atlas/internal/core/alert"
)

// MetricsProvider scores a node on how many enabled threshold rules its
// latest samples currently violate — evaluated directly against the
// sample, independent of the alert engine's pending/firing hysteresis. This
// is deliberately a different signal from [AlertProvider]: a threshold
// breach that has not yet been sustained long enough to fire still degrades
// this signal, catching a developing problem before an alert would.
type MetricsProvider struct {
	// Rules is the alert rule read path, reused for its threshold rules.
	Rules AlertRules
	// Metrics is the same fleet metric read port the alert engine evaluates
	// threshold rules against — see
	// [github.com/hexane/atlas/internal/core/alert.MetricSource].
	Metrics alert.MetricSource
}

func (p MetricsProvider) Name() string { return "metrics" }

func (p MetricsProvider) Score(ctx context.Context, nodeID string) (Signal, error) {
	rules, err := p.Rules.ListRules(ctx)
	if err != nil {
		return Signal{}, err
	}

	var critical, warning, checked int
	for _, r := range rules {
		if !r.Enabled || r.Kind != alert.KindThreshold {
			continue
		}
		if r.NodeID != "" && r.NodeID != nodeID {
			continue
		}

		samples, err := p.Metrics.LatestForMetric(ctx, r.Metric, alert.DefaultLookback)
		if err != nil {
			return Signal{}, err
		}
		for _, s := range samples {
			if s.NodeID != nodeID {
				continue
			}
			checked++
			if r.Comparison.Evaluate(s.Value, r.Threshold) {
				if r.Severity == alert.SeverityCritical {
					critical++
				} else {
					warning++
				}
			}
		}
	}

	if checked == 0 {
		return Signal{Available: false, Detail: "no threshold rules to evaluate"}, nil
	}

	score := max(100.0-float64(critical)*40-float64(warning)*15, 0)
	return Signal{
		Score: score, Available: true,
		Detail: fmt.Sprintf("%d/%d rule checks violated", critical+warning, checked),
	}, nil
}
