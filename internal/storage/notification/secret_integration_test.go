//go:build integration

package notification_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corenotification "github.com/hexane/atlas/internal/core/notification"
)

// TestTeamsChannelSecretNeverInConfigColumn covers item 1e's first bullet
// for "teams": created the way step 7's fix requires — the URL (whose
// sig= query parameter is the credential) in ChannelInput.Secret, nothing
// of it in Config — the stored config column must carry no trace of the
// URL, the signature, or any other query value, and ChannelSecret must
// round-trip the exact secret JSON the sender decodes.
func TestTeamsChannelSecretNeverInConfigColumn(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	const sig = "qT3x9f0ZkY8n2hY4ndaBcde-S0meRealLookingSignature123"
	url := "https://prod-01.westus.logic.azure.com/workflows/abcd1234/triggers/manual/paths/invoke?sig=" + sig
	secret := corenotification.TeamsSecret{URL: url}
	secretJSON, err := jsonMarshal(secret)
	if err != nil {
		t.Fatalf("marshal secret fixture: %v", err)
	}

	c, err := repo.CreateChannel(ctx, corenotification.ChannelInput{
		Name: "teams-ops", Type: corenotification.ChannelTeams, Enabled: true,
		MinSeverity: corenotification.SeverityWarning,
		Config:      map[string]any{}, // non-secret config: teams has no fields at all
		Secret:      ptr(secretJSON),
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	var configText string
	if err := pool.QueryRow(ctx, `SELECT config::text FROM notification_channels WHERE id = $1`, c.ID).Scan(&configText); err != nil {
		t.Fatalf("read config column: %v", err)
	}
	for _, leak := range []string{url, sig, "https://"} {
		if strings.Contains(configText, leak) {
			t.Errorf("config column = %q, leaks %q", configText, leak)
		}
	}

	got, err := repo.ChannelSecret(ctx, c.ID)
	if err != nil {
		t.Fatalf("ChannelSecret: %v", err)
	}
	if got != secretJSON {
		t.Errorf("ChannelSecret round-trip = %q, want %q", got, secretJSON)
	}
	decoded, err := corenotification.DecodeTeamsSecret(got)
	if err != nil {
		t.Fatalf("DecodeTeamsSecret: %v", err)
	}
	if decoded.URL != url {
		t.Errorf("decoded url = %q, want %q", decoded.URL, url)
	}
}

// TestWebhookChannelSecretNeverInConfigColumn is the "webhook" analogue:
// the url and headers (Authorization in particular) live in the secret;
// Config keeps only the template and content type.
func TestWebhookChannelSecretNeverInConfigColumn(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()

	const bearer = "Bearer sk_live_51H8xyzSECRETVALUE"
	url := "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
	secret := corenotification.WebhookSecret{URL: url, Headers: map[string]string{"Authorization": bearer}}
	secretJSON, err := jsonMarshal(secret)
	if err != nil {
		t.Fatalf("marshal secret fixture: %v", err)
	}

	c, err := repo.CreateChannel(ctx, corenotification.ChannelInput{
		Name: "webhook-ops", Type: corenotification.ChannelWebhook, Enabled: true,
		MinSeverity: corenotification.SeverityWarning,
		Config:      map[string]any{"template": `{"text": {{.msg}}}`, "content_type": "application/json"},
		Secret:      ptr(secretJSON),
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	var configText string
	if err := pool.QueryRow(ctx, `SELECT config::text FROM notification_channels WHERE id = $1`, c.ID).Scan(&configText); err != nil {
		t.Fatalf("read config column: %v", err)
	}
	for _, leak := range []string{url, bearer, "sk_live_51H8xyzSECRETVALUE", "hooks.slack.com"} {
		if strings.Contains(configText, leak) {
			t.Errorf("config column = %q, leaks %q", configText, leak)
		}
	}
	// The template itself is legitimately in config — only the url/headers
	// must be absent.
	if !strings.Contains(configText, "text") {
		t.Errorf("config column = %q, lost the legitimate non-secret template", configText)
	}

	got, err := repo.ChannelSecret(ctx, c.ID)
	if err != nil {
		t.Fatalf("ChannelSecret: %v", err)
	}
	if got != secretJSON {
		t.Errorf("ChannelSecret round-trip = %q, want %q", got, secretJSON)
	}
	decoded, err := corenotification.DecodeWebhookSecret(got)
	if err != nil {
		t.Fatalf("DecodeWebhookSecret: %v", err)
	}
	if decoded.URL != url || decoded.Headers["Authorization"] != bearer {
		t.Errorf("decoded = %+v", decoded)
	}
}

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
