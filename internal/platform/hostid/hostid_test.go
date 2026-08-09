package hostid_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/platform/hostid"
)

// fakeFS builds Options backed by an in-memory filesystem, so tests never
// touch the real /etc/machine-id or the user's config directory.
type fakeFS struct {
	files  map[string]string
	denied map[string]bool // paths that reject writes
}

func newFakeFS(files map[string]string) *fakeFS {
	if files == nil {
		files = map[string]string{}
	}
	return &fakeFS{files: files, denied: map[string]bool{}}
}

func (f *fakeFS) options() hostid.Options {
	return hostid.Options{
		Hostname: func() (string, error) { return "web-01", nil },
		ReadFile: func(path string) ([]byte, error) {
			if v, ok := f.files[path]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(path string, data []byte, _ os.FileMode) error {
			if f.denied[path] {
				return os.ErrPermission
			}
			f.files[path] = string(data)
			return nil
		},
		MachineIDPaths: []string{"/etc/machine-id", "/var/lib/dbus/machine-id"},
	}
}

const validMachineID = "3f2b7c1a9d8e4f0b6a5c3e2d1f0a9b8c"

func TestConfiguredIDWinsOverEverything(t *testing.T) {
	t.Parallel()

	fs := newFakeFS(map[string]string{"/etc/machine-id": validMachineID})
	opts := fs.options()
	opts.ConfiguredID = "  prod-web-01  "

	got, err := hostid.Resolve(opts)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.NodeID != "prod-web-01" {
		t.Errorf("NodeID = %q, want the configured id trimmed", got.NodeID)
	}
	if got.Source != hostid.SourceConfigured {
		t.Errorf("Source = %q, want configured", got.Source)
	}
	if got.Ephemeral() {
		t.Error("a configured id is never ephemeral")
	}
}

// Deriving from the machine id is what makes identity survive a restart with
// no state of Atlas's own.
func TestDerivesFromMachineID(t *testing.T) {
	t.Parallel()

	fs := newFakeFS(map[string]string{"/etc/machine-id": validMachineID + "\n"})

	got, err := hostid.Resolve(fs.options())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Source != hostid.SourceMachineID {
		t.Fatalf("Source = %q, want machine_id", got.Source)
	}
	if len(got.NodeID) != 32 {
		t.Errorf("NodeID = %q, want 32 characters", got.NodeID)
	}
	if got.Hostname != "web-01" {
		t.Errorf("Hostname = %q", got.Hostname)
	}
}

// The machine id is a fingerprinting vector; node ids appear in API responses,
// logs, and metric labels.
func TestNodeIDDoesNotRevealTheMachineID(t *testing.T) {
	t.Parallel()

	fs := newFakeFS(map[string]string{"/etc/machine-id": validMachineID})

	got, err := hostid.Resolve(fs.options())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.NodeID, validMachineID) {
		t.Fatalf("NodeID %q contains the raw machine id", got.NodeID)
	}
	// Nor any substantial prefix of it.
	if strings.Contains(got.NodeID, validMachineID[:16]) {
		t.Errorf("NodeID %q leaks half the machine id", got.NodeID)
	}
}

// Two hosts must never collide, and one host must never drift.
func TestDerivationIsStableAndUnique(t *testing.T) {
	t.Parallel()

	resolve := func(machineID string) string {
		t.Helper()
		fs := newFakeFS(map[string]string{"/etc/machine-id": machineID})
		got, err := hostid.Resolve(fs.options())
		if err != nil {
			t.Fatal(err)
		}
		return got.NodeID
	}

	first := resolve(validMachineID)
	second := resolve(validMachineID)
	if first != second {
		t.Errorf("same machine id produced %q then %q; identity must be stable", first, second)
	}

	other := resolve("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if other == first {
		t.Error("different machine ids produced the same node id")
	}
}

// The rename that fragments a machine's history is exactly what this prevents.
func TestIdentitySurvivesAHostnameChange(t *testing.T) {
	t.Parallel()

	fs := newFakeFS(map[string]string{"/etc/machine-id": validMachineID})

	before := fs.options()
	before.Hostname = func() (string, error) { return "old-name", nil }
	first, err := hostid.Resolve(before)
	if err != nil {
		t.Fatal(err)
	}

	after := fs.options()
	after.Hostname = func() (string, error) { return "renamed-during-the-incident", nil }
	second, err := hostid.Resolve(after)
	if err != nil {
		t.Fatal(err)
	}

	if first.NodeID != second.NodeID {
		t.Errorf("NodeID changed with the hostname: %q then %q", first.NodeID, second.NodeID)
	}
	if second.Hostname != "renamed-during-the-incident" {
		t.Errorf("Hostname = %q, want the current name for display", second.Hostname)
	}
}

func TestRejectsMalformedMachineID(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"too short":        "abc123",
		"not hex":          "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"empty":            "",
		"uppercase hex":    strings.ToUpper(validMachineID),
		"uuid with dashes": "3f2b7c1a-9d8e-4f0b-6a5c-3e2d1f0a9b8c",
	}

	for name, machineID := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeFS(map[string]string{"/etc/machine-id": machineID})
			opts := fs.options()
			opts.StateFile = "/tmp/atlas-node-id"

			got, err := hostid.Resolve(opts)
			if err != nil {
				t.Fatal(err)
			}
			// A file that is not what it claims must be ignored, not trusted:
			// deriving from it would yield a stable-looking id from unstable
			// input.
			if got.Source == hostid.SourceMachineID {
				t.Errorf("malformed machine id %q was accepted", machineID)
			}
		})
	}
}

func TestFallsBackToTheDBusPath(t *testing.T) {
	t.Parallel()

	fs := newFakeFS(map[string]string{"/var/lib/dbus/machine-id": validMachineID})

	got, err := hostid.Resolve(fs.options())
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != hostid.SourceMachineID {
		t.Errorf("Source = %q, want machine_id from the D-Bus path", got.Source)
	}
}

// Without this, a developer's every restart would appear as a new machine.
func TestGeneratesAndPersistsWhenNoMachineIDExists(t *testing.T) {
	t.Parallel()

	fs := newFakeFS(nil)
	opts := fs.options()
	opts.StateFile = "/var/lib/atlas/node-id"

	first, err := hostid.Resolve(opts)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if first.Source != hostid.SourceGenerated {
		t.Fatalf("Source = %q, want generated", first.Source)
	}
	if !first.Persisted || first.Ephemeral() {
		t.Error("a generated id that was written must not be reported as ephemeral")
	}
	if first.StateFile != "/var/lib/atlas/node-id" {
		t.Errorf("StateFile = %q", first.StateFile)
	}

	// The next start must read it back rather than generate a new one.
	second, err := hostid.Resolve(fs.options2(opts.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if second.NodeID != first.NodeID {
		t.Errorf("id changed across restarts: %q then %q", first.NodeID, second.NodeID)
	}
	if second.Source != hostid.SourceStateFile {
		t.Errorf("Source = %q, want state_file", second.Source)
	}
}

// options2 returns options pinned to a specific state file.
func (f *fakeFS) options2(stateFile string) hostid.Options {
	o := f.options()
	o.StateFile = stateFile
	return o
}

// A read-only container must still run, but the operator has to be told the
// history will fragment.
func TestReportsEphemeralWhenNothingIsWritable(t *testing.T) {
	t.Parallel()

	fs := newFakeFS(nil)
	opts := fs.options()
	opts.StateFile = "/read-only/node-id"
	fs.denied["/read-only/node-id"] = true

	got, err := hostid.Resolve(opts)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Source != hostid.SourceGenerated {
		t.Errorf("Source = %q, want generated", got.Source)
	}
	if got.Persisted {
		t.Error("Persisted = true although every write was refused")
	}
	if !got.Ephemeral() {
		t.Error("Ephemeral() = false; the caller would not know to warn")
	}
}

func TestGeneratedIDsAreUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 100)
	for range 100 {
		fs := newFakeFS(nil)
		opts := fs.options()
		opts.StateFile = "/read-only/node-id"
		fs.denied["/read-only/node-id"] = true

		got, err := hostid.Resolve(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[got.NodeID]; dup {
			t.Fatalf("duplicate generated id %q", got.NodeID)
		}
		seen[got.NodeID] = struct{}{}
	}
}

func TestDefaultStateFilePathsPreferSystemWide(t *testing.T) {
	t.Parallel()

	paths := hostid.DefaultStateFilePaths()
	if len(paths) == 0 {
		t.Fatal("no default state file paths")
	}
	if paths[0] != "/var/lib/atlas/node-id" {
		t.Errorf("first candidate = %q, want the system-wide path", paths[0])
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			t.Errorf("candidate %q is not absolute", p)
		}
	}
}

func TestHostnameFailureIsFatal(t *testing.T) {
	t.Parallel()

	fs := newFakeFS(map[string]string{"/etc/machine-id": validMachineID})
	opts := fs.options()
	opts.Hostname = func() (string, error) { return "", os.ErrPermission }

	if _, err := hostid.Resolve(opts); err == nil {
		t.Error("Resolve() succeeded although the hostname could not be read")
	}
}
