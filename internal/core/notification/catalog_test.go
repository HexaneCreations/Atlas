package notification

import (
	"context"
	"testing"
	"time"
)

func TestCatalogCoversEveryKnownTrigger(t *testing.T) {
	t.Parallel()
	if len(Catalog) != len(KnownTriggers) {
		t.Errorf("catalog has %d entries, %d triggers are known", len(Catalog), len(KnownTriggers))
	}
	seen := map[Trigger]bool{}
	for _, e := range Catalog {
		if !KnownTriggers[e.Key] || seen[e.Key] {
			t.Errorf("catalog entry %q is unknown or repeated", e.Key)
		}
		seen[e.Key] = true
		if e.Area == "" || e.Title == "" || e.Description == "" || !e.DefaultSeverity.Valid() {
			t.Errorf("catalog entry %q is incomplete: %+v", e.Key, e)
		}
	}
}

func TestResolveEnabledOrder(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	entry := CatalogEntry{Key: TriggerDiskPct, DefaultEnabled: true}
	for _, c := range []struct {
		name             string
		global, override *bool
		want             bool
		source           EnabledSource
	}{
		{"override wins over global", &yes, &no, false, SourceNode},
		{"override enables over a disabled global", &no, &yes, true, SourceNode},
		{"global when no override", &no, nil, false, SourceGlobal},
		{"catalog when neither", nil, nil, true, SourceCatalog},
	} {
		if got, src := ResolveEnabled(entry, c.global, c.override); got != c.want || src != c.source {
			t.Errorf("%s: %v %s, want %v %s", c.name, got, src, c.want, c.source)
		}
	}
}

func TestNodeRulesReadsOnlyThatNodesOverride(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	specs := []RuleSpec{
		{Trigger: TriggerDiskPct, Enabled: &yes},
		{Trigger: TriggerDiskPct, NodeID: "n1", Enabled: &no},
		{Trigger: TriggerRAMPct, NodeID: "n2", Enabled: &no},
		{Trigger: TriggerRAMPct, Enabled: &yes},
		{Trigger: TriggerNodeDown, NodeID: "n1"}, // override of other fields only
	}
	got := map[Trigger]NodeRule{}
	for _, r := range NodeRules(specs, "n1") {
		got[r.Key] = r
	}
	if len(got) != len(Catalog)-2 {
		t.Fatalf("got %d rules, want every node catalog entry", len(got))
	}
	if _, ok := got[TriggerWebsiteDown]; ok {
		t.Error("NodeRules lists a monitor rule")
	}
	if r := got[TriggerDiskPct]; r.Enabled || r.Source != SourceNode {
		t.Errorf("disk_pct = %+v, want disabled by the node", r)
	}
	if r := got[TriggerRAMPct]; !r.Enabled || r.Source != SourceGlobal || r.Override != nil {
		t.Errorf("ram_pct = %+v, want another node's override ignored", r)
	}
	if r := got[TriggerNodeDown]; !r.Enabled || r.Source != SourceCatalog {
		t.Errorf("node_down = %+v, want the catalog default with no enabled override", r)
	}
}

func TestParseRuleKey(t *testing.T) {
	t.Parallel()
	if k, err := ParseRuleKey("container_exit"); err != nil || k != TriggerContainerExit {
		t.Errorf("container_exit: %v %v", k, err)
	}
	for _, bad := range []string{"", "cpu_pct", "Disk_pct"} {
		if _, err := ParseRuleKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// --- evaluator -------------------------------------------------------------

func TestDisabledNodeRuleDoesNotFireAndOtherNodesStillDo(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: start}, {NodeID: "n2", LastSeenAt: start}}, "")
	no := false
	h.rules.setNodeEnabled(TriggerDiskPct, "n1", &no)
	for range 10 {
		now := h.clock.now()
		for _, n := range []string{"n1", "n2"} {
			h.nodes.nodes[n].LastSeenAt = now
		}
		h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/", 95, now), diskSample("n2", "/", 95, now)})
		h.eval.EvaluateOnce(context.Background())
		h.clock.advance(time.Minute)
	}
	sent := h.notifier.all()
	if len(sent) != 1 || sent[0].NodeID != "n2" {
		t.Fatalf("sent = %+v, want one firing for n2 only", sent)
	}
	if _, found, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", "/"); found {
		t.Error("a disabled rule left state for n1")
	}
	if h.rules.lists != 10 {
		t.Errorf("rules read %d times in 10 ticks, want once per tick", h.rules.lists)
	}
}

func TestGloballyDisabledRuleFiresWhereTheNodeEnablesIt(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: start}, {NodeID: "n2", LastSeenAt: start}}, "")
	r := h.rules.rules[TriggerDiskPct]
	r.Enabled = false
	h.rules.rules[TriggerDiskPct] = r
	yes := true
	h.rules.setNodeEnabled(TriggerDiskPct, "n1", &yes)
	for range 10 {
		now := h.clock.now()
		h.nodes.nodes["n1"].LastSeenAt, h.nodes.nodes["n2"].LastSeenAt = now, now
		h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/", 95, now), diskSample("n2", "/", 95, now)})
		h.eval.EvaluateOnce(context.Background())
		h.clock.advance(time.Minute)
	}
	if sent := h.notifier.all(); len(sent) != 1 || sent[0].NodeID != "n1" {
		t.Fatalf("sent = %+v, want one firing for n1 only", sent)
	}
}

func TestDisablingAFiringRuleResetsSilently(t *testing.T) {
	for _, trigger := range []Trigger{TriggerNodeDown, TriggerContainerExit} {
		t.Run(string(trigger), func(t *testing.T) {
			h := newWorkloadHarness(t)
			h.containers["web"] = fakeContainer{up: 1}
			h.tick()
			if trigger == TriggerNodeDown {
				for range 15 { // silent past NodeDownAfter plus the 480s hold
					h.eval.EvaluateOnce(context.Background())
					h.clock.advance(time.Minute)
				}
			} else {
				h.containers["web"] = fakeContainer{up: 0, exitCode: 137}
				h.tick()
			}
			subject := ""
			if trigger == TriggerContainerExit {
				subject = "web"
			}
			if s := h.state(trigger, subject); s.State != StateFiring {
				t.Fatalf("%s = %s, want firing before disabling", trigger, s.State)
			}
			sentBefore := len(h.notifier.all())

			no := false
			h.rules.setNodeEnabled(trigger, "n1", &no)
			h.ticks(10) // the node reports again and the container comes back
			h.containers["web"] = fakeContainer{up: 1}
			h.ticks(10)

			if s := h.state(trigger, subject); s.State != StateOK || s.Severity != "" {
				t.Errorf("%s = %+v, want silently ok", trigger, s)
			}
			if got := len(h.notifier.all()); got != sentBefore {
				t.Errorf("sent %d more notifications after disabling, want none (no recovered either)", got-sentBefore)
			}
		})
	}
}

// A container that exited while its rule was disabled is not announced when
// the rule is enabled again: the first observation after re-enabling is a
// baseline.
func TestReenablingStartsFromACleanBaseline(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1, restarts: 1}
	h.tick()
	no := false
	h.rules.setNodeEnabled(TriggerContainerExit, "n1", &no)
	h.rules.setNodeEnabled(TriggerContainerRestart, "n1", &no)
	h.tick()
	if _, seen := h.state(TriggerContainerExit, "web").Baseline[baselineSeenUp]; seen {
		t.Error("seen_up kept while disabled")
	}
	h.containers["web"] = fakeContainer{up: 0, exitCode: 137, restarts: 4}
	h.ticks(2)

	h.rules.setNodeEnabled(TriggerContainerExit, "n1", nil)
	h.rules.setNodeEnabled(TriggerContainerRestart, "n1", nil)
	h.ticks(3)
	h.wantKinds()

	// From here on it behaves normally again.
	h.containers["web"] = fakeContainer{up: 1, restarts: 4}
	h.tick()
	h.containers["web"] = fakeContainer{up: 0, exitCode: 137, restarts: 5}
	h.tick()
	h.wantKinds(KindFiring, KindFiring)
}
