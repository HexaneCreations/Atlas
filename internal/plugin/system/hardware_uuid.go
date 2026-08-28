package system

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// dmiProductUUIDPath is Linux's SMBIOS/DMI product UUID. A package variable
// rather than a constant only so tests can point the reader at a fake tree
// and prove it does not scavenge sibling identifiers.
var dmiProductUUIDPath = "/sys/class/dmi/id/product_uuid"

// readHardwareUUID returns the machine's raw hardware UUID: the SMBIOS/DMI
// product UUID on Linux, the IOPlatformUUID on macOS.
//
// It deliberately has NO fallback to /etc/machine-id or /proc/sys/kernel/
// random/boot_id -- unlike gopsutil's host.HostID, which chains through all
// three. The entire point of this identifier is to be hardware-rooted and
// separate from the machine-id that node_id is already derived from (see
// internal/platform/hostid), so a value scavenged from machine-id would be
// worse than nothing. Anything it cannot read -- an unprivileged agent on
// Linux, a container or non-x86 host with no DMI table, a platform that is
// neither Linux nor macOS -- yields "", never an error and never a
// substitute.
func readHardwareUUID(ctx context.Context) string {
	switch runtime.GOOS {
	case "linux":
		return readDMIProductUUID(dmiProductUUIDPath)
	case "darwin":
		return readIOPlatformUUID(ctx)
	default:
		return ""
	}
}

// readDMIProductUUID reads exactly one file and normalises it. Every failure
// mode -- absent, unreadable, empty, malformed -- collapses to "".
func readDMIProductUUID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return normaliseUUID(string(data))
}

// readIOPlatformUUID parses `ioreg`'s IOPlatformExpertDevice record, the same
// source system_profiler and gopsutil use for the Mac hardware UUID.
func readIOPlatformUUID(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		_, rest, ok := strings.Cut(line, `"IOPlatformUUID" = "`)
		if !ok {
			continue
		}
		if end := strings.IndexByte(rest, '"'); end >= 0 {
			return normaliseUUID(rest[:end])
		}
	}
	return ""
}

func normaliseUUID(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
