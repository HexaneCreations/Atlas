package notification

import (
	"slices"

	"github.com/hexane/atlas/internal/platform/errs"
)

// RuleArea groups catalog entries for presentation.
type RuleArea string

const (
	AreaAvailability RuleArea = "availability"
	AreaResources    RuleArea = "resources"
	AreaWorkloads    RuleArea = "workloads"
)

// CatalogEntry describes one alert rule. Adding a trigger is one entry here,
// its evaluator function, and a migration seeding its global rule row (a
// test fails if the seed is missing).
type CatalogEntry struct {
	Key             Trigger
	Area            RuleArea
	Title           string
	Description     string
	DefaultSeverity Severity
	DefaultEnabled  bool
}

// Catalog is every alert rule Atlas evaluates, in presentation order.
var Catalog = []CatalogEntry{
	{
		Key: TriggerNodeDown, Area: AreaAvailability, Title: "Node unreachable",
		Description:     "The node has stopped reporting to the control plane.",
		DefaultSeverity: SeverityCritical, DefaultEnabled: true,
	},
	{
		Key: TriggerDiskPct, Area: AreaResources, Title: "Disk usage",
		Description:     "A filesystem is filling up past the warning or critical threshold.",
		DefaultSeverity: SeverityWarning, DefaultEnabled: true,
	},
	{
		Key: TriggerRAMPct, Area: AreaResources, Title: "Memory usage",
		Description:     "Memory use is past the warning or critical threshold.",
		DefaultSeverity: SeverityWarning, DefaultEnabled: true,
	},
	{
		Key: TriggerContainerExit, Area: AreaWorkloads, Title: "Container exited",
		Description:     "A container that was running stopped with a non-zero exit code.",
		DefaultSeverity: SeverityCritical, DefaultEnabled: true,
	},
	{
		Key: TriggerContainerRestart, Area: AreaWorkloads, Title: "Container restarted",
		Description:     "A container's restart count went up.",
		DefaultSeverity: SeverityWarning, DefaultEnabled: true,
	},
	{
		Key: TriggerServiceFailed, Area: AreaWorkloads, Title: "Service down",
		Description:     "A systemd unit that was running is no longer active.",
		DefaultSeverity: SeverityCritical, DefaultEnabled: true,
	},
}

// CatalogEntryFor returns the entry for key.
func CatalogEntryFor(key Trigger) (CatalogEntry, bool) {
	i := slices.IndexFunc(Catalog, func(e CatalogEntry) bool { return e.Key == key })
	if i < 0 {
		return CatalogEntry{}, false
	}
	return Catalog[i], true
}

// EnabledSource is where a node's effective enabled value comes from.
type EnabledSource string

const (
	SourceNode    EnabledSource = "node"
	SourceGlobal  EnabledSource = "global"
	SourceCatalog EnabledSource = "catalog"
)

// NodeRule is one catalog entry as it applies to one node.
type NodeRule struct {
	CatalogEntry
	// GlobalEnabled is the global rule row's value, nil when there is none.
	GlobalEnabled *bool
	// Override is the node's own enabled value, nil to inherit.
	Override *bool
	Enabled  bool
	Source   EnabledSource
}

// ResolveEnabled applies the resolution order: the node override, else the
// global rule, else the catalog default.
func ResolveEnabled(entry CatalogEntry, global, override *bool) (bool, EnabledSource) {
	switch {
	case override != nil:
		return *override, SourceNode
	case global != nil:
		return *global, SourceGlobal
	default:
		return entry.DefaultEnabled, SourceCatalog
	}
}

// NodeRules resolves every catalog entry for nodeID from the stored rule
// rows (any mix of global rows and overrides, for any nodes).
func NodeRules(specs []RuleSpec, nodeID string) []NodeRule {
	out := make([]NodeRule, 0, len(Catalog))
	for _, entry := range Catalog {
		r := NodeRule{CatalogEntry: entry}
		for _, s := range specs {
			if s.Trigger != entry.Key {
				continue
			}
			if s.IsGlobal() {
				r.GlobalEnabled = s.Enabled
			} else if s.NodeID == nodeID {
				r.Override = s.Enabled
			}
		}
		r.Enabled, r.Source = ResolveEnabled(entry, r.GlobalEnabled, r.Override)
		out = append(out, r)
	}
	return out
}

// ParseRuleKey validates a rule key against the catalog.
func ParseRuleKey(key string) (Trigger, error) {
	if _, ok := CatalogEntryFor(Trigger(key)); !ok {
		return "", errs.New(errs.CodeNotFound, "unknown alert rule %q", key).
			WithOp("notification.ParseRuleKey").WithDetail("rule_key", key)
	}
	return Trigger(key), nil
}

// ruleSet is one tick's view of every stored rule, so the evaluator reads
// the rules table once per tick instead of once per (trigger, node).
type ruleSet struct {
	global    map[Trigger]RuleSpec
	overrides map[Trigger]map[string]RuleSpec
}

func newRuleSet(specs []RuleSpec) ruleSet {
	rs := ruleSet{global: map[Trigger]RuleSpec{}, overrides: map[Trigger]map[string]RuleSpec{}}
	for _, s := range specs {
		if s.IsGlobal() {
			rs.global[s.Trigger] = s
			continue
		}
		if rs.overrides[s.Trigger] == nil {
			rs.overrides[s.Trigger] = map[string]RuleSpec{}
		}
		rs.overrides[s.Trigger][s.NodeID] = s
	}
	return rs
}

// effective merges trigger's global rule with nodeID's override.
func (rs ruleSet) effective(trigger Trigger, nodeID string) (Rule, error) {
	global, ok := rs.global[trigger]
	if !ok {
		return Rule{}, errs.New(errs.CodeNotFound, "trigger %q has no global rule", trigger).WithOp("notification.ruleSet")
	}
	var override *RuleSpec
	if o, ok := rs.overrides[trigger][nodeID]; ok {
		override = &o
	}
	return MergeRule(global, override, nodeID)
}
