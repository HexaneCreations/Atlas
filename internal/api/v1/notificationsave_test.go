package v1_test

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/core/notification"
)

func channelBody(t *testing.T, typ, secret string, config map[string]any, extra ...string) string {
	t.Helper()
	m := map[string]any{"name": "ops", "type": typ, "config": config}
	if secret != "" {
		m["secret"] = secret
	}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return string(mustJSON(t, m))
}

func (f *testSendFixture) send(method, path, body string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.url+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (f *testSendFixture) wantField(code int, raw, field string) {
	f.t.Helper()
	var out struct {
		Error struct {
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || code != http.StatusBadRequest {
		f.t.Fatalf("status %d: %s, want 400", code, raw)
	}
	if out.Error.Message == "" || out.Error.Details["field"] != field {
		f.t.Errorf("error = %+v, want field %q", out.Error, field)
	}
}

var webhookTemplate = map[string]any{"template": "{{.msg}}"}

// A bad channel is refused when it is saved, naming the field, whether it is
// being created or updated, and nothing is stored or sent.
func TestSavingAChannelValidatesItsDestination(t *testing.T) {
	secretFor := func(url string, headers map[string]string) string {
		return string(mustJSON(t, notification.WebhookSecret{URL: url, Headers: headers}))
	}
	hook := func(url string) string { return secretFor(url, nil) }
	ok := "https://hooks.example.com/services/" + secretPath
	tooMany := map[string]string{}
	for i := range notification.MaxHeaderCount + 1 {
		tooMany["X-"+strings.Repeat("a", i+1)] = "v"
	}
	for name, c := range map[string]struct{ body, field string }{
		"teams host":      {channelBody(t, "teams", hook(ok), nil), "url"},
		"teams suffix":    {channelBody(t, "teams", hook("https://evil-logic.azure.com.example.com/x"), nil), "url"},
		"http":            {channelBody(t, "webhook", hook("http://hooks.example.com/x"), webhookTemplate), "url"},
		"userinfo":        {channelBody(t, "webhook", hook("https://u:p@hooks.example.com/x"), webhookTemplate), "url"},
		"loopback":        {channelBody(t, "webhook", hook("https://127.0.0.1/x"), webhookTemplate), "url"},
		"private":         {channelBody(t, "webhook", hook("https://10.1.2.3/x"), webhookTemplate), "url"},
		"metadata":        {channelBody(t, "webhook", hook("https://169.254.169.254/x"), webhookTemplate), "url"},
		"numeric":         {channelBody(t, "webhook", hook("https://0x7f000001/x"), webhookTemplate), "url"},
		"localhost":       {channelBody(t, "webhook", hook("https://localhost/x"), webhookTemplate), "url"},
		"secret not json": {channelBody(t, "webhook", ok, webhookTemplate), "url"},
		"denied header":   {channelBody(t, "webhook", secretFor(ok, map[string]string{"cookie": "x"}), webhookTemplate), "headers"},
		"header chars":    {channelBody(t, "webhook", secretFor(ok, map[string]string{"X Space": "x"}), webhookTemplate), "headers"},
		"header value":    {channelBody(t, "webhook", secretFor(ok, map[string]string{"X-A": "a\x00b"}), webhookTemplate), "headers"},
		"header count":    {channelBody(t, "webhook", secretFor(ok, tooMany), webhookTemplate), "headers"},
		"header length":   {channelBody(t, "webhook", secretFor(ok, map[string]string{strings.Repeat("a", 129): "x"}), webhookTemplate), "headers"},
		"no template":     {channelBody(t, "webhook", hook(ok), nil), "template"},
		"bad template":    {channelBody(t, "webhook", hook(ok), map[string]any{"template": "{{.msg"}), "template"},
		"config url":      {channelBody(t, "webhook", hook(ok), map[string]any{"template": "x", "url": ok}), "config"},
		"teams config":    {channelBody(t, "teams", hook("https://logic.azure.com/x"), map[string]any{"url": ok}), "config"},
		"no name":         {`{"name":" ","type":"teams"}`, "name"},
		"unknown type":    {channelBody(t, "sms", hook(ok), nil), "type"},
		"min severity":    {channelBody(t, "webhook", hook(ok), webhookTemplate, "min_severity", "info"), "min_severity"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTestSendFixture(t)
			before := f.store.secrets["chan-1"]
			for _, r := range [][2]string{{http.MethodPost, "/notifications/channels"}, {http.MethodPut, "/notifications/channels/chan-1"}} {
				code, raw := f.send(r[0], r[1], c.body)
				f.wantField(code, raw, c.field)
				f.noLeak(raw)
			}
			if len(f.store.channels) != 1 || f.store.secrets["chan-1"] != before || f.sender.calls() != 0 {
				t.Error("a refused save stored or sent something")
			}
		})
	}
}

func TestCreatingAChannelNeedsADestination(t *testing.T) {
	f := newTestSendFixture(t)
	code, raw := f.send(http.MethodPost, "/notifications/channels", channelBody(t, "teams", "", nil))
	f.wantField(code, raw, "url")
	code, raw = f.send(http.MethodPost, "/notifications/channels", channelBody(t, "email", "", map[string]any{"host": "smtp.example.com"}))
	if code != http.StatusCreated {
		t.Errorf("email: %d %s", code, raw)
	}
}

// Unknown fields and malformed JSON are refused with a fixed text.
func TestSavingAChannelDecodesStrictly(t *testing.T) {
	f := newTestSendFixture(t)
	good := channelBody(t, "webhook", webhookSecret("https://hooks.example.com/"+secretPath), webhookTemplate)
	for name, body := range map[string]string{
		"unknown field": strings.Replace(good, `{"`, `{"hooks.example.com":"`+secretPath+`","`, 1),
		"draft field":   strings.Replace(good, `{"`, `{"channel_id":"chan-1","`, 1),
		"not json":      "https://hooks.example.com/" + secretPath,
		"wrong type":    strings.Replace(good, `"ops"`, `["`+secretPath+`"]`, 1),
		"empty":         "",
	} {
		for _, r := range [][2]string{{http.MethodPost, "/notifications/channels"}, {http.MethodPut, "/notifications/channels/chan-1"}} {
			code, raw := f.send(r[0], r[1], body)
			if code != http.StatusBadRequest || !strings.Contains(raw, `"message":"invalid request body"`) {
				t.Errorf("%s %s: %d %s", name, r[0], code, raw)
			}
			f.noLeak(raw)
		}
	}
}

// Without a new secret only the non-secret config is validated, and the
// stored secret, whatever it is, is untouched.
func TestUpdatingAChannelWithoutASecret(t *testing.T) {
	f := newTestSendFixture(t)
	f.store.secrets["chan-1"] = "not valid any more"
	for _, secret := range []string{"", " "} {
		code, raw := f.send(http.MethodPut, "/notifications/channels/chan-1", channelBody(t, "webhook", secret, map[string]any{"template": "new"}))
		if code != http.StatusOK || f.store.secrets["chan-1"] != "not valid any more" || f.store.channels["chan-1"].Config["template"] != "new" {
			t.Errorf("secret %q: %d %s", secret, code, raw)
		}
	}
	code, raw := f.send(http.MethodPut, "/notifications/channels/chan-1", channelBody(t, "webhook", "", nil))
	f.wantField(code, raw, "template")
	code, raw = f.send(http.MethodPut, "/notifications/channels/chan-1", channelBody(t, "teams", "", nil))
	f.wantField(code, raw, "type")
}

// What an edit leaves empty is kept, for a save and for a test alike, except
// that stored header values never go to a new URL.
func TestEditKeepsTheEmptyPartsOfTheSecret(t *testing.T) {
	const stored = `{"url":"https://hooks.example.com/services/` + secretPath + `","headers":{"X-Api":"hdr-value-99","X-Old":"old"}}`
	for name, c := range map[string]struct {
		secret string
		want   map[string]any // nil: refused
		field  string
	}{
		"url kept, headers kept":     {`{"url":"","headers":{"X-Api":"","X-Old":""}}`, map[string]any{"X-Api": "hdr-value-99", "X-Old": "old"}, ""},
		"one replaced, one added":    {`{"url":"","headers":{"X-Api":"","X-Old":"new","X-New":"n"}}`, map[string]any{"X-Api": "hdr-value-99", "X-Old": "new", "X-New": "n"}, ""},
		"one removed":                {`{"url":"","headers":{"X-Api":""}}`, map[string]any{"X-Api": "hdr-value-99"}, ""},
		"all removed":                {`{"url":""}`, map[string]any{}, ""},
		"unknown kept header":        {`{"url":"","headers":{"X-Nope":""}}`, nil, "headers"},
		"new url with kept header":   {`{"url":"https://attacker.example.net/","headers":{"X-Api":""}}`, nil, "headers"},
		"kept url but denied header": {`{"url":"","headers":{"Cookie":"x"}}`, nil, "headers"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTestSendFixture(t)
			f.store.secrets["chan-1"] = stored
			code, raw := f.send(http.MethodPost, "/notifications/channels/test", channelBody(t, "webhook", c.secret, webhookTemplate, "channel_id", "chan-1"))
			if c.want == nil {
				f.wantField(code, raw, c.field)
			} else if code != http.StatusOK {
				t.Fatalf("test: %d %s", code, raw)
			}
			f.noLeak(raw)
			code, raw = f.send(http.MethodPut, "/notifications/channels/chan-1", channelBody(t, "webhook", c.secret, webhookTemplate))
			for _, leak := range []string{secretPath, "hdr-value-99"} {
				if strings.Contains(raw, leak) || strings.Contains(f.logs.String(), leak) {
					t.Errorf("%q leaked: %s", leak, raw)
				}
			}
			if c.want == nil {
				f.wantField(code, raw, c.field)
				if f.store.secrets["chan-1"] != stored || f.sender.calls() != 0 {
					t.Error("a refused edit changed or sent the secret")
				}
				return
			}
			if code != http.StatusOK {
				t.Fatalf("update: %d %s", code, raw)
			}
			for where, secret := range map[string]string{"saved": f.store.secrets["chan-1"], "tested": f.sender.secrets[0]} {
				var got struct {
					URL     string
					Headers map[string]any
				}
				if err := json.Unmarshal([]byte(secret), &got); err != nil {
					t.Fatal(err)
				}
				if got.URL != "https://hooks.example.com/services/"+secretPath || len(got.Headers) != len(c.want) {
					t.Errorf("%s = %+v", where, got)
				}
				for k, v := range c.want {
					if got.Headers[k] != v {
						t.Errorf("%s header %s = %v, want %v", where, k, got.Headers[k], v)
					}
				}
			}
		})
	}
}

// The response carries the host and the header names, never the rest.
func TestChannelResponseMasksTheDestination(t *testing.T) {
	f := newTestSendFixture(t)
	for _, path := range []string{"/notifications/channels/chan-1", "/notifications/channels"} {
		code, raw := f.send(http.MethodGet, path, "")
		if code != http.StatusOK || !strings.Contains(raw, `"destination_host":"hooks.example.com","header_names":["X-Api"]`) {
			t.Errorf("%s: %d %s", path, code, raw)
		}
		if strings.Contains(raw, secretPath) || strings.Contains(raw, "hdr-value-99") {
			t.Errorf("%s leaks: %s", path, raw)
		}
	}
	f.store.secrets["chan-1"] = "unreadable"
	code, raw := f.send(http.MethodGet, "/notifications/channels/chan-1", "")
	var got v1.NotificationChannelResponse
	if err := json.Unmarshal([]byte(raw), &got); err != nil || code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	if !got.SecretConfigured || got.DestinationHost != "" || !reflect.DeepEqual(got.HeaderNames, []string{}) {
		t.Errorf("unreadable secret: %+v", got)
	}
}
