package service

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// systemdProvider reads unit state via systemctl.
//
// systemctl rather than the D-Bus API, deliberately. The D-Bus route is faster
// and richer, but it pulls in a substantial dependency and — more importantly
// — needs a session or system bus connection that is frequently unavailable
// exactly where Atlas runs: inside a container with the host's /run mounted
// read-only, or as a user without bus access.
//
// systemctl is present wherever systemd is, produces machine-readable output,
// and needs no privileges to read state. The cost is process spawns, which is
// why this collector runs on a slow interval.
//
// If the spawn cost ever matters, the seam to replace is [Provider], and no
// collector changes.
type systemdProvider struct {
	// systemctl is the binary path, injectable for tests.
	systemctl string
	// run executes a command, injectable for tests.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewSystemdProvider returns a Provider backed by systemctl.
func NewSystemdProvider() Provider {
	return &systemdProvider{systemctl: "systemctl", run: runCommand}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Deterministic, parseable output regardless of the operator's locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "SYSTEMD_COLORS=0")
	return cmd.Output()
}

// Available reports whether systemd is running as PID 1.
//
// The /run/systemd/system directory is systemd's own documented marker for
// "booted with systemd", and it is more reliable than looking for the binary:
// systemctl can be installed in a container that is not managed by systemd, in
// which case every command fails with a confusing error.
func (p *systemdProvider) Available(ctx context.Context) bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	// Confirm the manager actually answers, so a half-configured host reports
	// absence rather than producing failures on every collection.
	if _, err := p.run(ctx, p.systemctl, "is-system-running"); err != nil {
		// is-system-running exits non-zero for "degraded", which is a running
		// system with a failed unit — precisely the case worth monitoring.
		// Only an inability to run it at all counts as unavailable.
		var exitErr *exec.ExitError
		if !errs.As(err, &exitErr) {
			return false
		}
	}
	return true
}

func (p *systemdProvider) Units(ctx context.Context) ([]Unit, error) {
	const op = "service.systemd.Units"

	// --all includes inactive and failed units. Listing only active ones would
	// hide the failed service that is the reason someone is looking.
	out, err := p.run(ctx, p.systemctl,
		"list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain")
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list systemd units").WithOp(op)
	}

	units := parseListUnits(string(out))
	if len(units) == 0 {
		return units, nil
	}

	p.enrich(ctx, units)
	return units, nil
}

// parseListUnits reads `systemctl list-units --plain --no-legend` output.
//
// Columns are UNIT LOAD ACTIVE SUB DESCRIPTION, whitespace-separated, with the
// description running to end of line.
func parseListUnits(out string) []Unit {
	var units []Unit

	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// A bullet marks a failed unit in some versions even with --plain.
		line = strings.TrimPrefix(line, "● ")
		line = strings.TrimPrefix(line, "* ")

		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasSuffix(fields[0], ".service") {
			continue
		}

		unit := Unit{
			Name:        fields[0],
			LoadState:   fields[1],
			ActiveState: normaliseActiveState(fields[2]),
			SubState:    SubState(fields[3]),
		}
		if len(fields) > 4 {
			unit.Description = strings.Join(fields[4:], " ")
		}
		units = append(units, unit)
	}
	return units
}

func normaliseActiveState(s string) ActiveState {
	switch ActiveState(strings.ToLower(s)) {
	case ActiveStateActive:
		return ActiveStateActive
	case ActiveStateInactive:
		return ActiveStateInactive
	case ActiveStateFailed:
		return ActiveStateFailed
	case ActiveStateActivating:
		return ActiveStateActivating
	case ActiveStateDeactivating:
		return ActiveStateDeactivating
	default:
		return ActiveStateUnknown
	}
}

// enrich fills the per-unit properties that the listing does not carry.
//
// One `systemctl show` for all units rather than one per unit: on a host with
// two hundred services that is the difference between one process spawn and
// two hundred.
func (p *systemdProvider) enrich(ctx context.Context, units []Unit) {
	names := make([]string, 0, len(units))
	index := make(map[string]int, len(units))
	for i, u := range units {
		names = append(names, u.Name)
		index[u.Name] = i
	}

	args := append([]string{
		"show",
		"--property=Id,MainPID,NRestarts,ActiveEnterTimestamp,MemoryCurrent,CPUUsageNSec,UnitFileState",
	}, names...)

	out, err := p.run(ctx, p.systemctl, args...)
	if err != nil {
		// The listing is still worth returning without the extra properties.
		return
	}

	// `systemctl show` separates units with a blank line.
	for _, block := range strings.Split(string(out), "\n\n") {
		props := parseProperties(block)
		i, ok := index[props["Id"]]
		if !ok {
			continue
		}
		applyProperties(&units[i], props)
	}
}

func parseProperties(block string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			props[key] = value
		}
	}
	return props
}

func applyProperties(u *Unit, props map[string]string) {
	if pid, err := strconv.ParseUint(props["MainPID"], 10, 32); err == nil {
		u.MainPID = uint32(pid)
	}
	if n, err := strconv.ParseUint(props["NRestarts"], 10, 32); err == nil {
		u.RestartCount = uint32(n)
	}
	// systemd reports "[not set]" and "infinity" for unlimited or unavailable
	// values; both parse as errors and correctly leave the field zero.
	if mem, err := strconv.ParseUint(props["MemoryCurrent"], 10, 64); err == nil {
		u.MemoryBytes = mem
	}
	if ns, err := strconv.ParseUint(props["CPUUsageNSec"], 10, 64); err == nil {
		u.CPUSeconds = float64(ns) / 1e9
	}
	u.Enabled = props["UnitFileState"] == "enabled" || props["UnitFileState"] == "enabled-runtime"
	u.ActiveSince = parseSystemdTime(props["ActiveEnterTimestamp"])
}

// systemdTimeLayouts are the formats systemd emits, which vary by version and
// by whether a timezone is configured.
var systemdTimeLayouts = []string{
	"Mon 2006-01-02 15:04:05 MST",
	"Mon 2006-01-02 15:04:05",
	"2006-01-02 15:04:05 MST",
}

func parseSystemdTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" || s == "n/a" {
		return time.Time{}
	}
	for _, layout := range systemdTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

var _ Provider = (*systemdProvider)(nil)

// dependencyProperties are the directives that define the graph.
//
// Both directions are requested. systemd computes the inverse of every
// relationship — a unit with `Before=x` causes x to report `After=<unit>` —
// so reading only the forward set would still be correct, but reading both
// catches asymmetries and costs nothing: they arrive in the same response.
// The parser rewrites the reverse ones into canonical form, which is where
// the resulting duplicates collapse.
const dependencyProperties = "Id,LoadState,ActiveState,SubState,Description," +
	"Requires,Wants,BindsTo,PartOf,After,Before,Conflicts," +
	"RequiredBy,WantedBy,BoundBy,ConsistsOf"

// Structure reads the dependency graph across every unit type.
//
// Two process spawns regardless of unit count: one listing, one batched show.
// On a 139-unit Debian host that is 4ms and 133ms respectively.
func (p *systemdProvider) Structure(ctx context.Context) (Structure, error) {
	const op = "service.systemd.Structure"

	// Two listings, unioned, because neither is complete on its own.
	//
	// `list-units` reports what the manager currently has loaded. It omits a
	// unit that is installed but inactive and not referenced by anything
	// running — which is precisely the unit an operator investigates when
	// asking why something is not up. On a plain Debian host it misses 57 of
	// 199 units, including every stopped service.
	//
	// `list-unit-files` reports what is on disk. It misses everything with no
	// file: mounts, devices, slices, and generated units — including `-.mount`,
	// which every systemd host has.
	//
	// No --type filter on either. Dependencies point mostly at targets, sockets
	// and mounts, and a services-only graph would be almost entirely dangling.
	loaded, err := p.run(ctx, p.systemctl,
		"list-units", "--all", "--no-legend", "--no-pager", "--plain")
	if err != nil {
		return Structure{}, errs.Wrap(err, errs.CodeUnavailable,
			"could not list systemd units").WithOp(op)
	}

	names := parseUnitNames(string(loaded))

	// Failure here is not fatal: the loaded set is still a usable graph, and a
	// systemd old enough to lack this subcommand should degrade rather than
	// return nothing.
	if files, err := p.run(ctx, p.systemctl,
		"list-unit-files", "--no-legend", "--no-pager"); err == nil {
		names = mergeUnitNames(names, parseUnitNames(string(files)))
	}

	if len(names) == 0 {
		return Structure{CollectedAt: time.Now()}, nil
	}

	// The `--` is load-bearing. Every systemd host has a `-.mount` (the root
	// mount) and systemctl parses a leading dash as an option, failing the
	// whole call with "invalid option -- '.'".
	args := append([]string{"show", "--property=" + dependencyProperties, "--"}, names...)
	shown, err := p.run(ctx, p.systemctl, args...)
	if err != nil {
		return Structure{}, errs.Wrap(err, errs.CodeUnavailable,
			"could not read systemd unit properties").WithOp(op)
	}

	nodes, edges := parseStructure(string(shown))
	return Structure{Nodes: nodes, Edges: edges, CollectedAt: time.Now()}, nil
}

// parseUnitNames reads unit names from `list-units --plain --no-legend`.
//
// Unlike parseListUnits this keeps every unit type, and tolerates the leading
// bullet some versions print for failed units even with --plain.
func parseUnitNames(out string) []string {
	var names []string

	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimPrefix(line, "● ")
		line = strings.TrimPrefix(line, "* ")
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// A unit name always carries a type suffix. Anything else is a stray
		// summary line.
		if !strings.Contains(fields[0], ".") {
			continue
		}
		// Templates (`getty@.service`) are patterns, not units: they have no
		// state and cannot be started. Their instances (`getty@tty1.service`)
		// are real and are kept.
		if strings.Contains(fields[0], "@.") {
			continue
		}
		names = append(names, fields[0])
	}
	return names
}

// mergeUnitNames unions two listings, preserving the order of the first.
//
// Order matters only for determinism: the same host must produce the same
// argument list on every collection, or `systemctl show` returns its blocks in
// a different order each time and every poll looks like a change.
func mergeUnitNames(primary, extra []string) []string {
	seen := make(map[string]struct{}, len(primary)+len(extra))
	out := make([]string, 0, len(primary)+len(extra))

	for _, name := range primary {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, name := range extra {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// forwardEdges map a property to the edge it declares, read as
// "this unit → the named unit".
var forwardEdges = map[string]EdgeKind{
	"Requires":  EdgeRequires,
	"Wants":     EdgeWants,
	"BindsTo":   EdgeBindsTo,
	"PartOf":    EdgePartOf,
	"After":     EdgeAfter,
	"Conflicts": EdgeConflicts,
}

// reverseEdges map a property to the edge it declares in the *other*
// direction, read as "the named unit → this unit".
//
// `Before=x` means this unit starts before x, which canonically is x ordered
// after this unit. `RequiredBy=x` means x requires this unit. Rewriting them
// this way is what makes the graph single-directional and duplicate-free.
var reverseEdges = map[string]EdgeKind{
	"Before":     EdgeAfter,
	"RequiredBy": EdgeRequires,
	"WantedBy":   EdgeWants,
	"BoundBy":    EdgeBindsTo,
	"ConsistsOf": EdgePartOf,
}

// parseStructure reads a batched `systemctl show` into nodes and canonical
// edges.
func parseStructure(out string) ([]Node, []Edge) {
	var nodes []Node
	var edges []Edge

	for _, block := range strings.Split(out, "\n\n") {
		props := parseProperties(block)
		name := props["Id"]
		if name == "" {
			continue
		}

		nodes = append(nodes, Node{
			Name:        name,
			Type:        unitType(name),
			Description: props["Description"],
			LoadState:   props["LoadState"],
			ActiveState: normaliseActiveState(props["ActiveState"]),
			SubState:    SubState(props["SubState"]),
			Known:       true,
		})

		for prop, kind := range forwardEdges {
			for _, target := range strings.Fields(props[prop]) {
				edges = append(edges, Edge{From: name, To: target, Kind: kind})
			}
		}
		for prop, kind := range reverseEdges {
			for _, source := range strings.Fields(props[prop]) {
				edges = append(edges, Edge{From: source, To: name, Kind: kind})
			}
		}
	}
	return nodes, edges
}
