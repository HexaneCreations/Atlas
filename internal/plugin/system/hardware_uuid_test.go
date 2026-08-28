package system

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadDMIProductUUID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	valid := write("valid", "4C4C4544-0034-3910-8053-B4C04F303232\n")
	if got := readDMIProductUUID(valid); got != "4c4c4544-0034-3910-8053-b4c04f303232" {
		t.Errorf("valid uuid = %q, want lowercased trimmed", got)
	}

	if got := readDMIProductUUID(write("empty", "")); got != "" {
		t.Errorf("empty file = %q, want \"\"", got)
	}
	if got := readDMIProductUUID(write("blank", "   \n\t")); got != "" {
		t.Errorf("whitespace-only file = %q, want \"\"", got)
	}
	if got := readDMIProductUUID(filepath.Join(dir, "does-not-exist")); got != "" {
		t.Errorf("missing file = %q, want \"\"", got)
	}
	if got := readDMIProductUUID(dir); got != "" {
		t.Errorf("directory path = %q, want \"\"", got)
	}
}

// The design hinges on this: the hardware-UUID read must never substitute a
// value scavenged from /etc/machine-id or boot_id, the way gopsutil's
// host.HostID chains through them. There is no such fallback in the code
// path; this proves it by placing those sibling files next to a missing
// product_uuid and asserting the result is still empty.
func TestReadHardwareUUIDHasNoMachineIDFallback(t *testing.T) {
	dir := t.TempDir()
	// Everything gopsutil's HostID would fall back to, present and readable.
	if err := os.WriteFile(filepath.Join(dir, "machine-id"), []byte("3f2b7c1a9d8e4f0b6a5c3e2d1f0a9b8c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "boot_id"), []byte("11111111-2222-3333-4444-555555555555\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingProductUUID := filepath.Join(dir, "product_uuid")

	if got := readDMIProductUUID(missingProductUUID); got != "" {
		t.Fatalf("readDMIProductUUID scavenged a sibling identifier: %q", got)
	}

	// And through the platform entry point: on Linux, pointing the reader at
	// the missing path must yield "" even though this host has a real
	// /etc/machine-id the fallback-chain version would have used.
	if runtime.GOOS == "linux" {
		restore := dmiProductUUIDPath
		dmiProductUUIDPath = missingProductUUID
		defer func() { dmiProductUUIDPath = restore }()
		if got := readHardwareUUID(context.Background()); got != "" {
			t.Fatalf("readHardwareUUID fell back to another identifier: %q", got)
		}
	}
}

func TestReadHardwareUUIDUnsupportedPlatform(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		t.Skipf("this host is %s; the unsupported-platform branch is unreachable here", runtime.GOOS)
	}
	if got := readHardwareUUID(context.Background()); got != "" {
		t.Errorf("readHardwareUUID on %s = %q, want \"\"", runtime.GOOS, got)
	}
}
