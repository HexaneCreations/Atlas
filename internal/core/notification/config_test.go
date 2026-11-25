package notification

import (
	"fmt"
	"strings"
	"testing"
)

func TestTeamsSecretValidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		s    TeamsSecret
		ok   bool
	}{
		{"valid logic.azure.com", TeamsSecret{URL: "https://prod-01.westus.logic.azure.com/workflows/x/triggers/manual/paths/invoke?sig=abc"}, true},
		{"valid api.powerplatform.com", TeamsSecret{URL: "https://prod-02.eastus.api.powerplatform.com/x?sig=abc"}, true},
		{"valid webhook.office.com", TeamsSecret{URL: "https://outlook.webhook.office.com/webhookb2/x@y/IncomingWebhook/z"}, true},
		{"exact suffix host with no subdomain", TeamsSecret{URL: "https://logic.azure.com/x"}, true},
		{"empty url", TeamsSecret{URL: ""}, false},
		{"http not https", TeamsSecret{URL: "http://logic.azure.com/x"}, false},
		{"disallowed host", TeamsSecret{URL: "https://evil.example.com/x"}, false},
		{"host suffix as substring, not a suffix", TeamsSecret{URL: "https://notlogic.azure.com.evil.test/x"}, false},
		{"userinfo in url", TeamsSecret{URL: "https://user:pass@logic.azure.com/x"}, false},
		{"too long", TeamsSecret{URL: "https://logic.azure.com/" + strings.Repeat("a", MaxDestinationURLLength)}, false},
		{"newline in url", TeamsSecret{URL: "https://logic.azure.com/x\r\nSet-Cookie: evil"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("Validate accepted an invalid secret")
			}
		})
	}
}

func TestDecodeTeamsSecret(t *testing.T) {
	t.Parallel()
	s, err := DecodeTeamsSecret(`{"url":"https://prod.logic.azure.com/x?sig=abc"}`)
	if err != nil {
		t.Fatalf("DecodeTeamsSecret: %v", err)
	}
	if s.URL != "https://prod.logic.azure.com/x?sig=abc" {
		t.Errorf("URL = %q", s.URL)
	}

	for _, tc := range []string{"", "not json", "{}", `{"url":""}`, `{"not_url":"x"}`} {
		if _, err := DecodeTeamsSecret(tc); err == nil {
			t.Errorf("DecodeTeamsSecret(%q): accepted", tc)
		}
	}
}

func TestTeamsSecretMaskedNeverShowsPathOrQuery(t *testing.T) {
	t.Parallel()
	s := TeamsSecret{URL: "https://prod.logic.azure.com/workflows/abc/triggers/manual/paths/invoke?sig=supersecret"}
	masked := s.Masked()
	url, _ := masked["url"].(string)
	if strings.Contains(url, "supersecret") || strings.Contains(url, "workflows/abc") {
		t.Errorf("Masked url = %q, leaks path or query", url)
	}
	if !strings.HasPrefix(url, "https://prod.logic.azure.com/") {
		t.Errorf("Masked url = %q, want scheme and host preserved", url)
	}
}

func TestWebhookConfigValidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  WebhookConfig
		ok   bool
	}{
		{"valid", WebhookConfig{Template: `{"text": {{.msg}}}`}, true},
		{"empty template", WebhookConfig{Template: ""}, false},
		{"template too large", WebhookConfig{Template: strings.Repeat("a", MaxTemplateLength+1)}, false},
		{"malformed template", WebhookConfig{Template: `{{.msg`}, false},
		{"template references an unsupported variable", WebhookConfig{Template: `{{.notAVariable}}`}, true}, // parses fine; missingkey=error fires at Execute, not Parse
		{"CRLF in content_type", WebhookConfig{Template: "x", ContentType: "application/json\r\nX-Injected: evil"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("Validate accepted an invalid config")
			}
		})
	}
}

// WebhookConfig no longer carries the destination or headers: nothing in
// its Validate, Masked, or decode path should look at them even if a
// caller mistakenly leaves them in Channel.Config (which, pre-fix, is
// exactly where they lived in plaintext).
func TestWebhookConfigIgnoresURLAndHeaders(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeWebhookConfig(map[string]any{
		"template": `{{.msg}}`,
		"url":      "https://leaked.example.test/should-be-ignored",
		"headers":  map[string]any{"Authorization": "Bearer leaked"},
	})
	if err != nil {
		t.Fatalf("DecodeWebhookConfig: %v", err)
	}
	if cfg.Template != "{{.msg}}" {
		t.Errorf("Template = %q", cfg.Template)
	}
	for k, v := range cfg.Masked() {
		if s, ok := v.(string); ok && (strings.Contains(s, "leaked") || strings.Contains(s, "Authorization")) {
			t.Errorf("Masked[%q] = %q: a url/headers key in Config leaked through", k, s)
		}
	}
}

func TestWebhookConfigMasked(t *testing.T) {
	t.Parallel()
	cfg := WebhookConfig{Template: "0123456789", ContentType: "application/json"}
	masked := cfg.Masked()
	if masked["content_type"] != "application/json" {
		t.Errorf("content_type = %v", masked["content_type"])
	}
	if masked["template_size"] != 10 {
		t.Errorf("template_size = %v, want 10", masked["template_size"])
	}
	if _, has := masked["template"]; has {
		t.Error("Masked must not include the template body itself")
	}
}

func TestWebhookSecretValidate(t *testing.T) {
	t.Parallel()
	validHeaders := map[string]string{"Authorization": "Bearer abc", "X-Custom": "value"}
	for _, tc := range []struct {
		name string
		s    WebhookSecret
		ok   bool
	}{
		{"valid, no headers", WebhookSecret{URL: "https://hooks.example.test/in"}, true},
		{"valid with allowed headers incl Authorization", WebhookSecret{URL: "https://hooks.example.test/in", Headers: validHeaders}, true},
		{"empty url", WebhookSecret{URL: ""}, false},
		{"http not https", WebhookSecret{URL: "http://hooks.example.test/in"}, false},
		{"not restricted to any host allowlist", WebhookSecret{URL: "https://anything.example.com/in"}, true},
		{"userinfo in url", WebhookSecret{URL: "https://user:pass@hooks.example.test/in"}, false},
		{"slack-style path token", WebhookSecret{URL: "https://hooks.slack.com/services/T000/B000/XXXXXXXXXXXXXXXXXXXXXXXX"}, true},
		{"discord-style path token", WebhookSecret{URL: "https://discord.com/api/webhooks/123456789012345678/AbCdEf"}, true},
		{"denied header Host", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{"Host": "evil.test"}}, false},
		{"denied header Content-Length", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{"Content-Length": "0"}}, false},
		{"denied header Transfer-Encoding", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{"Transfer-Encoding": "chunked"}}, false},
		{"denied header Connection", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{"Connection": "keep-alive"}}, false},
		{"denied header Cookie", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{"Cookie": "a=b"}}, false},
		{"denied header matched case-insensitively", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{"cOOkie": "a=b"}}, false},
		{"CRLF in header value", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{"X-Custom": "a\r\nX-Injected: evil"}}, false},
		{"CRLF in header name", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{"X-Custom\r\nX-Injected": "a"}}, false},
		{"too many headers", WebhookSecret{URL: "https://hooks.example.test/in", Headers: manyHeaders(MaxHeaderCount + 1)}, false},
		{"header name too long", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{strings.Repeat("a", MaxHeaderNameLength+1): "v"}}, false},
		{"header value too long", WebhookSecret{URL: "https://hooks.example.test/in", Headers: map[string]string{"X-Custom": strings.Repeat("a", MaxHeaderValueLength+1)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("Validate accepted an invalid secret")
			}
		})
	}
}

func manyHeaders(n int) map[string]string {
	m := make(map[string]string, n)
	for i := range n {
		m[fmt.Sprintf("X-Header-%d", i)] = "v"
	}
	return m
}

func TestDecodeWebhookSecret(t *testing.T) {
	t.Parallel()
	s, err := DecodeWebhookSecret(`{"url":"https://hooks.example.test/in","headers":{"Authorization":"Bearer abc"}}`)
	if err != nil {
		t.Fatalf("DecodeWebhookSecret: %v", err)
	}
	if s.URL != "https://hooks.example.test/in" || s.Headers["Authorization"] != "Bearer abc" {
		t.Errorf("decoded = %+v", s)
	}

	for _, tc := range []string{"", "not json", "{}", `{"url":""}`} {
		if _, err := DecodeWebhookSecret(tc); err == nil {
			t.Errorf("DecodeWebhookSecret(%q): accepted", tc)
		}
	}
}

func TestWebhookSecretMaskedNeverShowsHeaderValues(t *testing.T) {
	t.Parallel()
	s := WebhookSecret{
		URL:     "https://hooks.example.test/in/signedpath?token=abc",
		Headers: map[string]string{"Authorization": "Bearer supersecret", "X-Custom": "also-secret"},
	}
	masked := s.Masked()
	for _, v := range masked {
		if str, ok := v.(string); ok {
			if strings.Contains(str, "supersecret") || strings.Contains(str, "also-secret") ||
				strings.Contains(str, "signedpath") || strings.Contains(str, "token=abc") {
				t.Errorf("Masked leaked a secret value: %v", masked)
			}
		}
	}
	names, _ := masked["header_names"].([]string)
	if len(names) != 2 {
		t.Errorf("header_names = %v, want 2 names, no values", names)
	}
}
