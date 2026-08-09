// Package hostid resolves the stable identity of the machine being observed.
//
// Every observation Atlas stores is attributed to a node id, and that id has to
// outlive the things people casually change. A hostname is renamed; a DHCP
// lease moves; a VM is re-imaged and comes back with the same role. If identity
// tracked any of those, a machine's history would fragment into several
// apparent machines at exactly the moment an operator needed continuity — the
// week of the incident that caused the rename.
//
// So identity is resolved once at startup, in this order:
//
//  1. An explicitly configured id. Operators own their naming; nothing here
//     overrides an intentional choice.
//  2. A derivation from the OS machine id, where the OS provides one. Stable
//     across reboots, hostname changes, and network moves, and requires no
//     state of Atlas's own.
//  3. A previously persisted state file.
//  4. A freshly generated id, persisted for next time.
//
// # Why the machine id is hashed rather than used
//
// /etc/machine-id is a stable, globally unique identifier for the installation,
// and systemd's own documentation is explicit that it must be treated as
// confidential: exposing it lets anything that sees it correlate a machine
// across otherwise unrelated systems. Atlas puts node ids in API responses,
// logs, and metric labels.
//
// The value is therefore passed through HMAC-SHA256 under an application
// specific key. The result is stable and unique in exactly the way Atlas needs,
// and reveals nothing about the underlying machine id.
package hostid

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hmacKey namespaces the derivation. It is a domain separator, not a secret:
// its purpose is to ensure Atlas's node id cannot be correlated with an id
// derived from the same machine id by any other application.
//
// Changing this value changes every node id, orphaning all historical metrics
// from the machines that produced them. It must never change.
var hmacKey = []byte("atlas.node-id.v1")

// linuxMachineIDPaths are consulted in order.
//
// /etc/machine-id is systemd's canonical location. The D-Bus path predates it
// and is still present on some distributions and inside minimal containers.
var linuxMachineIDPaths = []string{
	"/etc/machine-id",
	"/var/lib/dbus/machine-id",
}

// Source records how an identity was obtained. It is reported in logs and on
// the API so an operator can tell a durable id from one that will not survive
// a container restart.
type Source string

const (
	// SourceConfigured means an operator supplied the id explicitly.
	SourceConfigured Source = "configured"
	// SourceMachineID means the id was derived from the OS machine id. The
	// most durable source: no Atlas state is involved.
	SourceMachineID Source = "machine_id"
	// SourceStateFile means the id was read from a file Atlas wrote earlier.
	SourceStateFile Source = "state_file"
	// SourceGenerated means the id was created during this startup. Durable
	// only if it was also persisted — check [Identity.Persisted].
	SourceGenerated Source = "generated"
)

// Identity is the resolved identity of the observed machine.
type Identity struct {
	// NodeID is the stable identifier. Hex-encoded, 32 characters.
	NodeID string
	// Hostname is the machine's current name, for display only. It may change
	// between runs without affecting NodeID — which is the entire point.
	Hostname string
	// Source records how NodeID was obtained.
	Source Source
	// Persisted reports whether the id will survive a restart. False means a
	// generated id could not be written anywhere, so the next start produces a
	// different node and fragments this machine's history.
	Persisted bool
	// StateFile is the path the id was read from or written to, if any.
	StateFile string
}

// Ephemeral reports whether this identity will be lost on restart.
func (i Identity) Ephemeral() bool { return i.Source == SourceGenerated && !i.Persisted }

// Options controls resolution. The function fields exist so tests can run
// against a synthetic filesystem rather than the real one.
type Options struct {
	// ConfiguredID, when non-empty, is used verbatim.
	ConfiguredID string
	// StateFile is where a generated id is persisted. Empty selects the first
	// writable path from [DefaultStateFilePaths].
	StateFile string

	// MachineIDPaths overrides the OS machine id locations. Empty uses the
	// platform defaults.
	MachineIDPaths []string

	// Hostname defaults to os.Hostname.
	Hostname func() (string, error)
	// ReadFile defaults to os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// WriteFile defaults to a helper that creates parent directories.
	WriteFile func(path string, data []byte, perm os.FileMode) error
}

// DefaultStateFilePaths returns candidate locations for the persisted id, most
// preferred first.
//
// The system path is tried first so that all users of a host share one
// identity. A per-user path follows, because a developer running Atlas without
// elevated privileges must still get a stable id across restarts — otherwise
// every local run would appear as a new machine and litter the database.
func DefaultStateFilePaths() []string {
	paths := []string{"/var/lib/atlas/node-id"}

	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		paths = append(paths, filepath.Join(dir, "atlas", "node-id"))
	}
	return paths
}

// Resolve determines this machine's identity.
//
// It returns an error only when the hostname cannot be read, which indicates a
// badly broken host. Every other failure degrades to the next source, because
// an Atlas that refuses to start over a missing state file would be useless in
// a read-only container.
func Resolve(opts Options) (Identity, error) {
	const op = "hostid.Resolve"

	if opts.Hostname == nil {
		opts.Hostname = os.Hostname
	}
	if opts.ReadFile == nil {
		opts.ReadFile = os.ReadFile
	}
	if opts.WriteFile == nil {
		opts.WriteFile = writeFileWithParents
	}
	if opts.MachineIDPaths == nil {
		opts.MachineIDPaths = linuxMachineIDPaths
	}

	hostname, err := opts.Hostname()
	if err != nil {
		return Identity{}, fmt.Errorf("%s: read hostname: %w", op, err)
	}

	if id := strings.TrimSpace(opts.ConfiguredID); id != "" {
		return Identity{
			NodeID: id, Hostname: hostname,
			Source: SourceConfigured, Persisted: true,
		}, nil
	}

	if raw, ok := readMachineID(opts); ok {
		return Identity{
			NodeID: derive(raw), Hostname: hostname,
			Source: SourceMachineID, Persisted: true,
		}, nil
	}

	candidates := stateFileCandidates(opts.StateFile)

	// An id already written by a previous run outranks a new one.
	for _, path := range candidates {
		if id, ok := readStateFile(opts, path); ok {
			return Identity{
				NodeID: id, Hostname: hostname,
				Source: SourceStateFile, Persisted: true, StateFile: path,
			}, nil
		}
	}

	id, err := generate()
	if err != nil {
		return Identity{}, fmt.Errorf("%s: generate node id: %w", op, err)
	}

	for _, path := range candidates {
		// 0o600: the id is not a secret, but it is an identifier that should
		// not be casually rewritten by other users on the host.
		if err := opts.WriteFile(path, []byte(id+"\n"), 0o600); err == nil {
			return Identity{
				NodeID: id, Hostname: hostname,
				Source: SourceGenerated, Persisted: true, StateFile: path,
			}, nil
		}
	}

	// Nowhere writable. Atlas still runs, but the caller is expected to warn:
	// this machine's history will fragment on every restart.
	return Identity{
		NodeID: id, Hostname: hostname,
		Source: SourceGenerated, Persisted: false,
	}, nil
}

func stateFileCandidates(configured string) []string {
	if configured != "" {
		return []string{configured}
	}
	return DefaultStateFilePaths()
}

// readMachineID returns the first machine id found, and whether one was.
func readMachineID(opts Options) (string, bool) {
	for _, path := range opts.MachineIDPaths {
		data, err := opts.ReadFile(path)
		if err != nil {
			continue
		}
		// systemd writes 32 lowercase hex characters. Anything else means a
		// file that is not what it claims to be, and deriving from it would
		// produce a stable-looking id from unstable input.
		id := strings.TrimSpace(string(data))
		if len(id) == 32 && isHex(id) {
			return id, true
		}
	}
	return "", false
}

func readStateFile(opts Options, path string) (string, bool) {
	data, err := opts.ReadFile(path)
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(data))
	if len(id) == 32 && isHex(id) {
		return id, true
	}
	return "", false
}

// derive turns a machine id into an Atlas node id that cannot be reversed.
func derive(machineID string) string {
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write([]byte(machineID))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

func generate() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func writeFileWithParents(path string, data []byte, perm os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, perm)
}
