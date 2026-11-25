package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// allowTestServerHost temporarily allowlists host (an httptest server's
// 127.0.0.1) so a test can exercise TeamsSender.Send end to end, including
// its own TeamsSecret.Validate call, against a local fixture server — a
// real Workflows host is never reachable from a test.
//
// Mutates the package-level TeamsAllowedHostSuffixes, so the calling test
// must not call t.Parallel(): every test in this package that only reads
// it (config_test.go's TeamsSecret tests included) does, and parallel
// tests never run concurrently with a non-parallel one, only with each
// other — so this stays race-free as long as callers stay sequential.
func allowTestServerHost(t *testing.T, host string) {
	t.Helper()
	orig := TeamsAllowedHostSuffixes
	TeamsAllowedHostSuffixes = append(append([]string{}, orig...), host)
	t.Cleanup(func() { TeamsAllowedHostSuffixes = orig })
}

// teamsSecretJSON builds the sealed-secret JSON a "teams" channel's
// decrypted secret_ciphertext decodes to.
func teamsSecretJSON(url string) string {
	return fmt.Sprintf(`{"url":%q}`, url)
}

// TestTeamsSenderPayloadShape is a snapshot test of the envelope and
// Adaptive Card shape the brief specifies: {"type":"message","attachments":
// [{"contentType":"application/vnd.microsoft.card.adaptive","contentUrl":
// null,"content": <card>}]}, with the card showing severity, trigger, node,
// and subject as facts and the delivery's title/message as its body.
func TestTeamsSenderPayloadShape(t *testing.T) {
	// Not t.Parallel(): mutates TeamsAllowedHostSuffixes, see
	// allowTestServerHost.
	var captured map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	allowTestServerHost(t, "127.0.0.1")

	// srv.Client() trusts the fixture server's self-signed certificate,
	// which a real Workflows destination would never present; this test
	// is about the payload shape and the real TeamsSecret.Validate path
	// (via the temporary host allowlist entry above), not certificate
	// trust, which the guarded client's own tests already cover.
	sender := &TeamsSender{client: srv.Client()}
	channel := Channel{Type: ChannelTeams, Config: map[string]any{}}
	d := Delivery{
		EventID: "evt-1", Kind: KindFiring, Trigger: TriggerDiskPct, NodeID: "node-7", Subject: "/var",
		Severity: SeverityCritical, Title: "disk_pct critical on node-7", Message: "disk usage is 95% on /var",
	}

	if err := sender.Send(context.Background(), channel, teamsSecretJSON(srv.URL), d); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if captured["type"] != "message" {
		t.Errorf("type = %v, want \"message\"", captured["type"])
	}
	attachments, ok := captured["attachments"].([]any)
	if !ok || len(attachments) != 1 {
		t.Fatalf("attachments = %v, want exactly one", captured["attachments"])
	}
	attachment, ok := attachments[0].(map[string]any)
	if !ok {
		t.Fatalf("attachment is not an object: %v", attachments[0])
	}
	if attachment["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("contentType = %v, want the adaptive card content type", attachment["contentType"])
	}
	if _, isNull := attachment["contentUrl"]; !isNull || attachment["contentUrl"] != nil {
		t.Errorf("contentUrl = %v, want null", attachment["contentUrl"])
	}
	content, ok := attachment["content"].(map[string]any)
	if !ok {
		t.Fatalf("content is not an object: %v", attachment["content"])
	}
	if content["type"] != "AdaptiveCard" {
		t.Errorf("content.type = %v, want AdaptiveCard", content["type"])
	}

	body, ok := content["body"].([]any)
	if !ok || len(body) < 2 {
		t.Fatalf("content.body = %v, want at least a heading and a FactSet", content["body"])
	}
	var factSet map[string]any
	for _, block := range body {
		b, ok := block.(map[string]any)
		if ok && b["type"] == "FactSet" {
			factSet = b
		}
	}
	if factSet == nil {
		t.Fatal("no FactSet block found in the card body")
	}
	facts, ok := factSet["facts"].([]any)
	if !ok {
		t.Fatalf("facts = %v, want a list", factSet["facts"])
	}
	want := map[string]string{"Severity": "critical", "Trigger": "disk_pct", "Node": "node-7", "Subject": "/var"}
	got := map[string]string{}
	for _, f := range facts {
		fact, ok := f.(map[string]any)
		if !ok {
			continue
		}
		title, _ := fact["title"].(string)
		value, _ := fact["value"].(string)
		got[title] = value
	}
	for title, wantValue := range want {
		if got[title] != wantValue {
			t.Errorf("fact %q = %q, want %q (facts: %v)", title, got[title], wantValue, got)
		}
	}

	bodyText := joinTextBlocks(body)
	if !strings.Contains(bodyText, d.Title) || !strings.Contains(bodyText, d.Message) {
		t.Errorf("card body text %q does not contain both the delivery's title and message", bodyText)
	}
}

func joinTextBlocks(body []any) string {
	var out string
	for _, block := range body {
		b, ok := block.(map[string]any)
		if !ok || b["type"] != "TextBlock" {
			continue
		}
		if text, ok := b["text"].(string); ok {
			out += text + "\n"
		}
	}
	return out
}

// TestTeamsSenderRejectsDisallowedHost proves the host allowlist is
// enforced at send time, not just by whatever created the channel.
func TestTeamsSenderRejectsDisallowedHost(t *testing.T) {
	t.Parallel()
	sender := NewTeamsSender()
	channel := Channel{Type: ChannelTeams, Config: map[string]any{}}
	err := sender.Send(context.Background(), channel, teamsSecretJSON("https://evil.example.com/webhook"), Delivery{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("expected a disallowed host to be rejected")
	}
}

// TestTeamsSenderTreatsRedirectAsFailure proves a 3xx response from the
// destination is reported as a failed delivery, not a success.
func TestTeamsSenderTreatsRedirectAsFailure(t *testing.T) {
	// Not t.Parallel(): mutates TeamsAllowedHostSuffixes, see
	// allowTestServerHost.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://logic.azure.com/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	allowTestServerHost(t, "127.0.0.1")

	// srv.Client() trusts the fixture's certificate but follows redirects
	// by default; production's guarded client never does (see
	// httpclient_test.go), so this test matches that by overriding
	// CheckRedirect — otherwise it would try to actually dial
	// logic.azure.com.
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	sender := &TeamsSender{client: client}
	channel := Channel{Type: ChannelTeams, Config: map[string]any{}}
	err := sender.Send(context.Background(), channel, teamsSecretJSON(srv.URL), Delivery{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("expected a redirect response to be treated as a failed delivery")
	}
}

// TestTeamsSenderNeverPutsTheURLInAnError proves item 2: a destination
// error carries the operation and the host, never the full URL — a real
// Workflows sig= would otherwise leak through a dial failure's error text.
// The SSRF guard's own refusal of loopback (deterministic, no real
// network dependency, see httpclient_test.go) is the dial failure used
// here; wrapSendError must scrub it the same as any other.
func TestTeamsSenderNeverPutsTheURLInAnError(t *testing.T) {
	// Not t.Parallel(): mutates TeamsAllowedHostSuffixes, see
	// allowTestServerHost.
	allowTestServerHost(t, "127.0.0.1")
	sender := NewTeamsSender()
	channel := Channel{Type: ChannelTeams, Config: map[string]any{}}
	const sig = "qT3x9f0ZkY8n2hY4ndaBcde-S0meRealLookingSignature123"
	secret := teamsSecretJSON("https://127.0.0.1:1/workflows/abcd1234/triggers/manual/paths/invoke?sig=" + sig)

	err := sender.Send(context.Background(), channel, secret, Delivery{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("expected the destination to fail (loopback is refused by the SSRF guard before any real dial)")
	}
	if strings.Contains(err.Error(), sig) || strings.Contains(err.Error(), "workflows/abcd1234") {
		t.Errorf("error leaked the url: %v", err)
	}
}
