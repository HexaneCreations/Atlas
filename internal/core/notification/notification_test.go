package notification

import (
	"reflect"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func globalDisk() RuleSpec {
	return RuleSpec{
		Trigger: TriggerDiskPct, Enabled: ptr(true), WarnThreshold: ptr(80.0), CritThreshold: ptr(90.0),
		ForSeconds: ptr(300), RecoverySeconds: ptr(120), Hysteresis: ptr(5.0), CooldownSeconds: ptr(1800),
		SendRecovery: ptr(true), SubjectExcludes: []string{},
	}
}

func TestMergeRuleAppliesOverridesPerField(t *testing.T) {
	t.Parallel()

	base := Rule{
		Trigger: TriggerDiskPct, NodeID: "n1", Enabled: true, WarnThreshold: ptr(80.0), CritThreshold: ptr(90.0),
		ForSeconds: 300, RecoverySeconds: 120, Hysteresis: 5, CooldownSeconds: 1800, SendRecovery: true,
		SubjectExcludes: []string{},
	}
	with := func(f func(*Rule)) Rule { r := base; f(&r); return r }

	for _, tc := range []struct {
		name     string
		override *RuleSpec
		want     Rule
	}{
		{"no override inherits everything", nil, base},
		{"empty override inherits everything", &RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1"}, base},
		{"warn only", &RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1", WarnThreshold: ptr(70.0)},
			with(func(r *Rule) { r.WarnThreshold = ptr(70.0); r.Overridden = []string{"warn_threshold"} })},
		{"crit only", &RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1", CritThreshold: ptr(97.0)},
			with(func(r *Rule) { r.CritThreshold = ptr(97.0); r.Overridden = []string{"crit_threshold"} })},
		{"disabled on one node", &RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1", Enabled: ptr(false)},
			with(func(r *Rule) { r.Enabled = false; r.Overridden = []string{"enabled"} })},
		{"zero values are overrides, not inherit", &RuleSpec{
			Trigger: TriggerDiskPct, NodeID: "n1", ForSeconds: ptr(0), Hysteresis: ptr(0.0), SendRecovery: ptr(false),
		}, with(func(r *Rule) {
			r.ForSeconds, r.Hysteresis, r.SendRecovery = 0, 0, false
			r.Overridden = []string{"for_seconds", "hysteresis", "send_recovery"}
		})},
		{"timings", &RuleSpec{
			Trigger: TriggerDiskPct, NodeID: "n1", RecoverySeconds: ptr(60), CooldownSeconds: ptr(600),
		}, with(func(r *Rule) {
			r.RecoverySeconds, r.CooldownSeconds = 60, 600
			r.Overridden = []string{"recovery_seconds", "cooldown_seconds"}
		})},
		{"excludes replace, empty clears", &RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1", SubjectExcludes: []string{"/boot"}},
			with(func(r *Rule) { r.SubjectExcludes = []string{"/boot"}; r.Overridden = []string{"subject_excludes"} })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MergeRule(globalDisk(), tc.override, "n1")
			if err != nil {
				t.Fatalf("MergeRule: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("MergeRule =\n %+v\nwant\n %+v", got, tc.want)
			}
		})
	}
}

// The merge does not alias the global spec: changing the result's
// thresholds leaves the global untouched.
func TestMergeRuleDoesNotAliasTheOverride(t *testing.T) {
	t.Parallel()
	o := &RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1", WarnThreshold: ptr(70.0)}
	got, err := MergeRule(globalDisk(), o, "n1")
	if err != nil {
		t.Fatal(err)
	}
	*got.WarnThreshold = 1
	if *o.WarnThreshold != 70 {
		t.Error("the merged rule aliases the override's threshold")
	}
}

func TestMergeRuleRejectsAnEffectiveWarnAboveCrit(t *testing.T) {
	t.Parallel()
	if _, err := MergeRule(globalDisk(), &RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1", WarnThreshold: ptr(95.0)}, "n1"); err == nil {
		t.Fatal("an override pushing warn above the inherited crit was accepted")
	}
}

func TestMergeRuleRejectsAnIncompleteGlobal(t *testing.T) {
	t.Parallel()
	g := globalDisk()
	g.CooldownSeconds = nil
	if _, err := MergeRule(g, nil, "n1"); err == nil {
		t.Fatal("an incomplete global rule was merged")
	}
}

func TestRuleSpecValidate(t *testing.T) {
	t.Parallel()
	nodeDown := RuleSpec{
		Trigger: TriggerNodeDown, Enabled: ptr(true), ForSeconds: ptr(300), RecoverySeconds: ptr(120),
		Hysteresis: ptr(0.0), CooldownSeconds: ptr(1800), SendRecovery: ptr(true), SubjectExcludes: []string{},
	}
	for _, tc := range []struct {
		name string
		spec RuleSpec
		ok   bool
	}{
		{"complete global disk", globalDisk(), true},
		{"global node_down without thresholds", nodeDown, true},
		{"node override with one field", RuleSpec{Trigger: TriggerRAMPct, NodeID: "n1", CritThreshold: ptr(99.0)}, true},
		{"unknown trigger", RuleSpec{Trigger: "cpu_pct", NodeID: "n1"}, false},
		{"global missing a field", func() RuleSpec { s := globalDisk(); s.SendRecovery = nil; return s }(), false},
		{"global disk missing thresholds", func() RuleSpec { s := globalDisk(); s.WarnThreshold = nil; return s }(), false},
		{"global missing excludes", func() RuleSpec { s := globalDisk(); s.SubjectExcludes = nil; return s }(), false},
		{"thresholds on node_down", RuleSpec{Trigger: TriggerNodeDown, NodeID: "n1", WarnThreshold: ptr(1.0)}, false},
		{"warn above crit", RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1", WarnThreshold: ptr(95.0), CritThreshold: ptr(90.0)}, false},
		{"negative for", RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1", ForSeconds: ptr(-1)}, false},
		{"negative hysteresis", RuleSpec{Trigger: TriggerDiskPct, NodeID: "n1", Hysteresis: ptr(-1.0)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("Validate accepted an invalid spec")
			}
		})
	}
}

func TestChannelInputValidate(t *testing.T) {
	t.Parallel()
	ok := ChannelInput{Name: "ops", Type: ChannelTeams, MinSeverity: SeverityWarning}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for name, in := range map[string]ChannelInput{
		"no name":      {Type: ChannelEmail, MinSeverity: SeverityWarning},
		"unknown type": {Name: "x", Type: "slack", MinSeverity: SeverityWarning},
		"bad severity": {Name: "x", Type: ChannelEmail, MinSeverity: "info"},
	} {
		if err := in.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Node event ids are the dedupe key stored in notification_deliveries; a
// change to their format would re-send every alert already delivered.
func TestEventIDFormatIsStable(t *testing.T) {
	t.Parallel()
	anchor := time.Unix(1764000000, 0)
	if got := eventID(TriggerDiskPct, "node-1", "/var", anchor, KindFiring, SeverityCritical); got != "notif-disk_pct-node-1-/var-1764000000-firing-critical" {
		t.Errorf("eventID = %q", got)
	}
	if got := eventID(TriggerNodeDown, "node-1", "", anchor, KindRecovered, SeverityCritical); got != "notif-node_down-node-1--1764000000-recovered-critical" {
		t.Errorf("eventID = %q", got)
	}
}
