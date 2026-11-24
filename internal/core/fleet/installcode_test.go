package fleet

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNewInstallCodeShapeAndHash(t *testing.T) {
	code, err := NewInstallCode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code.Plaintext, InstallCodePrefix) || len(code.Plaintext) != len(InstallCodePrefix)+64 {
		t.Fatalf("code %q has the wrong shape", code.Plaintext)
	}
	if !ValidInstallCodeFormat(code.Plaintext) {
		t.Fatal("a generated code fails its own format check")
	}
	if code.Hash != HashToken(code.Plaintext) || len(code.Hash) != 64 {
		t.Fatalf("hash = %q, want sha256 hex of the plaintext", code.Hash)
	}
	other, _ := NewInstallCode()
	if other.Plaintext == code.Plaintext {
		t.Fatal("two generated codes are equal")
	}
}

func TestInstallCodeFormatRejectsWrongPrefixAndShape(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	for _, bad := range []string{
		"",
		TokenPrefix + hex64,             // an enrollment token
		"atlas_installs_" + hex64,       // near-miss prefix
		InstallCodePrefix + hex64[:63],  // short
		InstallCodePrefix + hex64 + "0", // long
		InstallCodePrefix + strings.ToUpper(hex64),
		InstallCodePrefix + hex64[:62] + "zz", // not hex
		" " + InstallCodePrefix + hex64,
		InstallCodePrefix + hex64 + "\n",
	} {
		if ValidInstallCodeFormat(bad) {
			t.Errorf("ValidInstallCodeFormat(%q) = true", bad)
		}
	}
}

func TestNewNodeIDIs32Hex(t *testing.T) {
	id, err := NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if !nodeIDRE.MatchString(id) {
		t.Fatalf("node id %q is not 32 lowercase hex", id)
	}
}

func TestInstallCodeTTLBounds(t *testing.T) {
	minutes := func(n int) *int { return &n }
	spec := InstallCodeSpec{Environment: "production", CreatedBy: "u1"}
	if ttl, err := spec.TTL(); err != nil || ttl != 30*time.Minute {
		t.Fatalf("default TTL = %v, %v; want 30m", ttl, err)
	}
	for _, ok := range []int{1, 30, 60} {
		spec.TTLMinutes = minutes(ok)
		if ttl, err := spec.TTL(); err != nil || ttl != time.Duration(ok)*time.Minute {
			t.Errorf("TTL(%d) = %v, %v", ok, ttl, err)
		}
	}
	for _, bad := range []int{0, -5, 61, 1000} {
		spec.TTLMinutes = minutes(bad)
		if _, err := spec.TTL(); err == nil {
			t.Errorf("TTL(%d) accepted", bad)
		}
	}
	for _, env := range []string{"", "prod uction", "prod\n", "a'b", strings.Repeat("e", 65)} {
		if _, err := (InstallCodeSpec{Environment: env, CreatedBy: "u1"}).TTL(); err == nil {
			t.Errorf("environment %q accepted", env)
		}
	}
	if _, err := (InstallCodeSpec{Environment: "production"}).TTL(); err == nil {
		t.Error("a spec with no creator was accepted")
	}
}

func TestInstallCodeStatus(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)
	c := InstallCode{ExpiresAt: now.Add(time.Minute)}
	if got := c.Status(now); got != InstallCodePending {
		t.Errorf("status = %s, want pending", got)
	}
	if got := (InstallCode{ExpiresAt: past}).Status(now); got != InstallCodeExpired {
		t.Errorf("status = %s, want expired", got)
	}
	if got := (InstallCode{ExpiresAt: now.Add(time.Minute), RevokedAt: &past}).Status(now); got != InstallCodeRevoked {
		t.Errorf("status = %s, want revoked", got)
	}
	if got := (InstallCode{ExpiresAt: past, RedeemedAt: &past}).Status(now); got != InstallCodeRedeemed {
		t.Errorf("status = %s, want redeemed", got)
	}
}

// installScriptRegexes reads the allowlist regexes straight out of
// packaging/atlas-agent/install.sh, so this test fails if the script and
// the server ever disagree.
func installScriptRegexes(t *testing.T) map[string]*regexp.Regexp {
	t.Helper()
	src, err := os.ReadFile("../../../packaging/atlas-agent/install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	out := map[string]*regexp.Regexp{}
	for _, m := range regexp.MustCompile(`(?m)^([A-Z0-9_]+_RE)='([^']*)'$`).FindAllStringSubmatch(string(src), -1) {
		out[m[1]] = regexp.MustCompile(m[2])
	}
	for _, name := range []string{"URL_RE", "MULTIADDR_RE", "ENVIRONMENT_RE", "TOKEN_RE", "VERSION_RE"} {
		if out[name] == nil {
			t.Fatalf("install.sh defines no %s", name)
		}
	}
	return out
}

func TestServerAllowlistsAreInstallScriptAllowlists(t *testing.T) {
	script := installScriptRegexes(t)
	for name, ours := range map[string]*regexp.Regexp{
		"URL_RE": urlRE, "MULTIADDR_RE": multiaddrRE, "ENVIRONMENT_RE": environmentRE,
		"TOKEN_RE": tokenRE, "VERSION_RE": versionRE,
	} {
		if ours.String() != script[name].String() {
			t.Errorf("%s: server %q, install.sh %q", name, ours, script[name])
		}
	}
}

var testTarget = InstallTarget{
	Version:         "v1.4.0",
	ControlPlaneURL: "https://atlas.example.com",
	ServerAddr:      "/ip4/203.0.113.10/tcp/4102/p2p/12D3KooWD3eckifWpRn9wQpMG9R9hX3sD158z7EqHWmweQAJU5SA",
}

// Every value in the generated command passes the same check install.sh
// applies to its flag, and every value is single-quoted.
func TestInstallCommandValuesPassInstallScriptAllowlists(t *testing.T) {
	script := installScriptRegexes(t)
	code, _ := NewInstallCode()
	cmd, err := InstallCommand(testTarget, code.Plaintext)
	if err != nil {
		t.Fatal(err)
	}

	flagRE := map[string]string{
		"--version": "VERSION_RE", "--control-plane-url": "URL_RE",
		"--server-addr": "MULTIADDR_RE", "--install-code": "TOKEN_RE",
	}
	_, installArgs, found := strings.Cut(cmd, `sudo bash "$d/install.sh"`)
	if !found {
		t.Fatalf("command does not run the downloaded install.sh: %q", cmd)
	}
	seen := map[string]string{}
	for _, m := range regexp.MustCompile(`(--[a-z-]+) '([^']*)'`).FindAllStringSubmatch(installArgs, -1) {
		re, ok := flagRE[m[1]]
		if !ok {
			t.Errorf("unexpected quoted flag %s in %q", m[1], cmd)
			continue
		}
		if strings.ContainsAny(m[2], "\n\r") || !script[re].MatchString(m[2]) {
			t.Errorf("%s value %q fails install.sh %s", m[1], m[2], re)
		}
		seen[m[1]] = m[2]
	}
	for flag := range flagRE {
		if _, ok := seen[flag]; !ok {
			t.Errorf("command has no %s value: %q", flag, cmd)
		}
	}
	if seen["--install-code"] != code.Plaintext || seen["--version"] != testTarget.Version ||
		seen["--server-addr"] != testTarget.ServerAddr || seen["--control-plane-url"] != testTarget.ControlPlaneURL {
		t.Errorf("command carries the wrong values: %v", seen)
	}
	if !strings.Contains(cmd, " --transport libp2p") {
		t.Errorf("command does not select libp2p: %q", cmd)
	}
	scriptURL := regexp.MustCompile(`-o "\$d/install.sh" '([^']*)'`).FindStringSubmatch(cmd)
	if scriptURL == nil || !script["URL_RE"].MatchString(scriptURL[1]) ||
		!strings.Contains(scriptURL[1], "/"+testTarget.Version+"/packaging/atlas-agent/install.sh") {
		t.Errorf("install.sh download URL is missing, unpinned or not allowlisted: %q", cmd)
	}
	if strings.Count(cmd, "'")%2 != 0 {
		t.Errorf("unbalanced quotes: %q", cmd)
	}
}

func TestInstallCommandRefusesValuesTheInstallerWouldReject(t *testing.T) {
	code, _ := NewInstallCode()
	for name, mutate := range map[string]func(*InstallTarget){
		"dev version":         func(x *InstallTarget) { x.Version = "dev" },
		"describe version":    func(x *InstallTarget) { x.Version = "v1.4.0-3-gabcdef0" },
		"http url":            func(x *InstallTarget) { x.ControlPlaneURL = "http://atlas.example.com" },
		"url with quote":      func(x *InstallTarget) { x.ControlPlaneURL = "https://atlas.example.com/'x" },
		"url with newline":    func(x *InstallTarget) { x.ControlPlaneURL = "https://atlas.example.com\n" },
		"url with space":      func(x *InstallTarget) { x.ControlPlaneURL = "https://atlas example.com" },
		"addr without peer":   func(x *InstallTarget) { x.ServerAddr = "/ip4/203.0.113.10/tcp/4102" },
		"addr with semicolon": func(x *InstallTarget) { x.ServerAddr = testTarget.ServerAddr + ";id" },
	} {
		target := testTarget
		mutate(&target)
		if cmd, err := InstallCommand(target, code.Plaintext); err == nil {
			t.Errorf("%s: built %q", name, cmd)
		}
	}
	if _, err := InstallCommand(testTarget, "atlas_install_'; rm -rf /"); err == nil {
		t.Error("a malformed code was put into a command")
	}
}

func TestValidPeerID(t *testing.T) {
	if !ValidPeerID("12D3KooWD3eckifWpRn9wQpMG9R9hX3sD158z7EqHWmweQAJU5SA") {
		t.Error("a real peer id was rejected")
	}
	if !ValidPeerID("QmYyQSo1c1Ym7orWxLYvCrM2EmxFTANf8wXmmE7DWjhx5N") {
		t.Error("a sha2-256 peer id was rejected")
	}
	for _, bad := range []string{"", "12D3KooWD3eckifWpRn9wQpMG9R9hX3sD158z7EqHWmweQAJU5S", "not-a-peer", "12D3KooW'",
		"12D3KooWnope", "QmYyQSo1c1Ym7orWxLYvCrM2EmxFTANf8wXmmE7DWjhx5"} {
		if ValidPeerID(bad) {
			t.Errorf("ValidPeerID(%q) = true", bad)
		}
	}
}

func TestRedeemLimiterPerIPAndGlobal(t *testing.T) {
	now := time.Now()
	l := NewRedeemLimiter(3, 5)
	l.nowFunc = func() time.Time { return now }

	for i := range 3 {
		if !l.Allow("198.51.100.1") {
			t.Fatalf("attempt %d from one IP refused within its budget", i+1)
		}
	}
	if l.Allow("198.51.100.1") {
		t.Fatal("a 4th attempt from one IP was allowed")
	}
	// The refused attempt above still spent a global token: 4 of 5 used.
	if !l.Allow("198.51.100.2") {
		t.Fatal("a second IP was refused within the global budget")
	}
	if l.Allow("198.51.100.3") {
		t.Fatal("the global cap did not hold across IPs")
	}

	now = now.Add(time.Minute)
	if !l.Allow("198.51.100.1") {
		t.Fatal("budgets did not refill after a minute")
	}
}
