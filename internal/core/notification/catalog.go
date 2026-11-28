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
	AreaWebsites     RuleArea = "websites"
)

// SubjectKind is what a trigger watches: a node, or a website monitor.
type SubjectKind string

// The subject kinds.
const (
	SubjectNode    SubjectKind = "node"
	SubjectMonitor SubjectKind = "monitor"
)

// SubjectKind is the kind of subject t watches.
func (t Trigger) SubjectKind() SubjectKind {
	if t == TriggerWebsiteDown || t == TriggerWebsiteCertExpiry {
		return SubjectMonitor
	}
	return SubjectNode
}

// CatalogEntry describes one alert rule. Adding a trigger is one entry here,
// its evaluator function, and a migration seeding its global rule row (a
// test fails if the seed is missing).
type CatalogEntry struct {
	Key             Trigger
	Kind            SubjectKind
	Area            RuleArea
	Title           string
	Description     string
	DefaultSeverity Severity
	DefaultEnabled  bool
}

// Catalog is every alert rule Atlas evaluates, in presentation order.
var Catalog = []CatalogEntry{
	{
		Key: TriggerNodeDown, Kind: SubjectNode, Area: AreaAvailability, Title: "Node unreachable",
		Description:     "The node has stopped reporting to the control plane.",
		DefaultSeverity: SeverityCritical, DefaultEnabled: true,
	},
	{
		Key: TriggerDiskPct, Kind: SubjectNode, Area: AreaResources, Title: "Disk usage",
		Description:     "A filesystem is filling up past the warning or critical threshold.",
		DefaultSeverity: SeverityWarning, DefaultEnabled: true,
	},
	{
		Key: TriggerRAMPct, Kind: SubjectNode, Area: AreaResources, Title: "Memory usage",
		Description:     "Memory use is past the warning or critical threshold.",
		DefaultSeverity: SeverityWarning, DefaultEnabled: true,
	},
	{
		Key: TriggerContainerExit, Kind: SubjectNode, Area: AreaWorkloads, Title: "Container exited",
		Description:     "A container that was running stopped with a non-zero exit code.",
		DefaultSeverity: SeverityCritical, DefaultEnabled: true,
	},
	{
		Key: TriggerContainerRestart, Kind: SubjectNode, Area: AreaWorkloads, Title: "Container restarted",
		Description:     "A container's restart count went up.",
		DefaultSeverity: SeverityWarning, DefaultEnabled: true,
	},
	{
		Key: TriggerServiceFailed, Kind: SubjectNode, Area: AreaWorkloads, Title: "Service down",
		Description:     "A systemd unit that was running is no longer active.",
		DefaultSeverity: SeverityCritical, DefaultEnabled: true,
	},
	{
		Key: TriggerWebsiteDown, Kind: SubjectMonitor, Area: AreaWebsites, Title: "Website down",
		Description:     "The website failed more checks in a row than its retries allow.",
		DefaultSeverity: SeverityCritical, DefaultEnabled: true,
	},
	{
		Key: TriggerWebsiteCertExpiry, Kind: SubjectMonitor, Area: AreaWebsites, Title: "Certificate expiring",
		Description:     "The website's TLS certificate expires within its warning or critical number of days.",
		DefaultSeverity: SeverityWarning, DefaultEnabled: true,
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
	SourceMonitor EnabledSource = "monitor"
	SourceGlobal  EnabledSource = "global"
	SourceCatalog EnabledSource = "catalog"
)

// NodeRule is one catalog entry as it applies to one node, or (from
// [MonitorRules]) to one monitor.
type NodeRule struct {
	CatalogEntry
	// GlobalEnabled is the global rule row's value, nil when there is none.
	GlobalEnabled *bool
	// Override is the node's own enabled value, nil to inherit.
	Override *bool
	Enabled  bool
	Source   EnabledSource
}

// SubjectRule is a [NodeRule] whose subject may be either kind.
type SubjectRule = NodeRule

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

// NodeRules resolves every node catalog entry for nodeID from the stored
// rule rows (any mix of global rows and overrides, for any subjects).
// Monitor entries are left out.
func NodeRules(specs []RuleSpec, nodeID string) []NodeRule {
	return subjectRules(specs, SubjectNode, nodeID)
}

// MonitorRules is [NodeRules] for a website monitor: the monitor catalog
// entries only.
func MonitorRules(specs []RuleSpec, monitorID string) []SubjectRule {
	return subjectRules(specs, SubjectMonitor, MonitorScope(monitorID))
}

func subjectRules(specs []RuleSpec, kind SubjectKind, scope string) []NodeRule {
	out := make([]NodeRule, 0, len(Catalog))
	for _, entry := range Catalog {
		if entry.Kind != kind {
			continue
		}
		r := NodeRule{CatalogEntry: entry}
		for _, s := range specs {
			if s.Trigger != entry.Key {
				continue
			}
			if s.IsGlobal() {
				r.GlobalEnabled = s.Enabled
			} else if s.scope() == scope {
				r.Override = s.Enabled
			}
		}
		r.Enabled, r.Source = ResolveEnabled(entry, r.GlobalEnabled, r.Override)
		if r.Source == SourceNode && kind == SubjectMonitor {
			r.Source = SourceMonitor
		}
		out = append(out, r)
	}
	return out
}

// ParseRuleKey validates a node rule key against the catalog. A monitor
// rule's key is not found here: node endpoints never see those.
func ParseRuleKey(key string) (Trigger, error) {
	return ParseRuleKeyFor(SubjectNode, key)
}

// ParseRuleKeyFor validates a rule key for a subject kind.
func ParseRuleKeyFor(kind SubjectKind, key string) (Trigger, error) {
	if e, ok := CatalogEntryFor(Trigger(key)); !ok || e.Kind != kind {
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
		rs.overrides[s.Trigger][s.scope()] = s
	}
	return rs
}

// effective merges trigger's global rule with the override of nodeID, a
// node id or a [MonitorScope].
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
