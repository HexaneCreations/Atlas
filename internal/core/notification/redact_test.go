package notification

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		mustNot []string // substrings that must not survive
	}{
		{
			name:    "teams workflows sig",
			in:      "https://prod-01.westus.logic.azure.com:443/workflows/abcd1234/triggers/manual/paths/invoke?api-version=2016-06-01&sp=%2Ftriggers%2Fmanual%2Frun&sv=1.0&sig=qT3x9f0ZkY8n2hY4ndaBcde-S0meRealLookingSignature123",
			mustNot: []string{"qT3x9f0ZkY8n2hY4ndaBcde-S0meRealLookingSignature123"},
		},
		{
			name:    "power automate api_key",
			in:      "https://prod-02.eastus.api.powerplatform.com/powerautomate/automations/direct/workflows/zz/triggers/manual/paths/invoke/host/instance?api_key=AKIA_LOOKS_REAL_1234567890",
			mustNot: []string{"AKIA_LOOKS_REAL_1234567890"},
		},
		{
			name:    "generic webhook token and code",
			in:      "https://hooks.example.test/in/abc?token=sk_live_51H8xyzSECRETVALUE&code=oauthcode123&region=us",
			mustNot: []string{"sk_live_51H8xyzSECRETVALUE", "oauthcode123"},
		},
		{
			// A whole URL's path and query are reduced together: item 2
			// extends Redact to do this because a credential can live in
			// the *path* (Slack, Discord, a Teams workflow id), where a
			// query-parameter-only pass would miss it entirely. The cost
			// is that a harmless parameter next to a real one (api-version,
			// sv) is redacted along with it — an acceptable trade next to
			// leaking the one that mattered.
			name: "a whole url's path and query are reduced together, not just named params",
			in:   "https://logic.azure.com/x?api-version=2016-06-01&sv=1.0&sig=thesecret",
			want: "https://logic.azure.com/" + Placeholder,
		},
		{
			name:    "slack-style path-embedded token",
			in:      "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX",
			mustNot: []string{"T00000000", "XXXXXXXXXXXXXXXXXXXXXXXX"},
			want:    "https://hooks.slack.com/" + Placeholder,
		},
		{
			name:    "discord-style path-embedded token",
			in:      "https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
			mustNot: []string{"123456789012345678", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"},
			want:    "https://discord.com/" + Placeholder,
		},
		{
			name:    "slack token embedded in a url.Error message",
			in:      (&url.Error{Op: "Post", URL: "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX", Err: fmt.Errorf("dial tcp: connection refused")}).Error(),
			mustNot: []string{"T00000000", "XXXXXXXXXXXXXXXXXXXXXXXX"},
		},
		{
			name:    "discord token embedded in a response snippet",
			in:      `{"message": "unknown webhook", "url_tried": "https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"}`,
			mustNot: []string{"123456789012345678", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"},
		},
		{
			name:    "wrapped url.Error embeds the full url with sig",
			in:      (&url.Error{Op: "Post", URL: "https://webhook.office.com/webhookb2/x@y/IncomingWebhook/z?sig=EmbeddedSecretValue", Err: fmt.Errorf("dial tcp: connection refused")}).Error(),
			mustNot: []string{"EmbeddedSecretValue"},
		},
		{
			name:    "response snippet echoing the request url",
			in:      `{"error":"bad request for https://logic.azure.com/workflows/a/triggers/manual/paths/invoke?sig=LeakedInBody"}`,
			mustNot: []string{"LeakedInBody"},
		},
		{
			name:    "authorization header text",
			in:      "request failed, Authorization: Bearer abcdef0123456789",
			mustNot: []string{"abcdef0123456789"},
		},
		{
			name:    "custom secret header name in query form",
			in:      "https://hooks.example.test/in?secret=hunter2&key=anotherone&signature=sigvalue123",
			mustNot: []string{"hunter2", "anotherone", "sigvalue123"},
		},
		{
			name: "idempotent on already-redacted text",
			in:   Placeholder,
			want: Placeholder,
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			for _, bad := range tc.mustNot {
				if strings.Contains(got, bad) {
					t.Errorf("Redact(%q) = %q, still contains %q", tc.in, got, bad)
				}
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Idempotent: redacting again must not change the result further.
			if again := Redact(got); again != got {
				t.Errorf("Redact is not idempotent: Redact(%q) = %q, then Redact(that) = %q", tc.in, got, again)
			}
		})
	}
}
