package monitor

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

func valid() Monitor {
	m := Defaults()
	m.Name, m.URL = "Shop", "https://shop.example.com/health"
	return m
}

func field(err error) string {
	var e *errs.Error
	if !errs.As(err, &e) {
		return ""
	}
	return fmt.Sprint(e.Details["field"])
}

func TestValidate(t *testing.T) {
	t.Parallel()
	if err := Validate(valid(), false); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for _, c := range []struct {
		name  string
		edit  func(*Monitor)
		field string
	}{
		{"empty name", func(m *Monitor) { m.Name = " " }, "name"},
		{"bad method", func(m *Monitor) { m.Method = "PUT" }, "method"},
		{"interval below 20s", func(m *Monitor) { m.IntervalSeconds = 19 }, "interval_seconds"},
		{"retry interval below 20s", func(m *Monitor) { m.RetryIntervalSeconds = 5 }, "retry_interval_seconds"},
		{"timeout not shorter than interval", func(m *Monitor) { m.IntervalSeconds, m.TimeoutSeconds = 30, 30 }, "timeout_seconds"},
		{"too many retries", func(m *Monitor) { m.Retries = 11 }, "retries"},
		{"too many redirects", func(m *Monitor) { m.MaxRedirects = 11 }, "max_redirects"},
		{"recovery zero", func(m *Monitor) { m.RecoverySuccesses = 0 }, "recovery_successes"},
		{"keyword on HEAD", func(m *Monitor) { m.Method, m.Keyword = MethodHead, "ok" }, "keyword"},
		{"cert critical not below warning", func(m *Monitor) { m.CertWarnDays, m.CertCritDays = 3, 3 }, "cert_crit_days"},
		{"bad status range", func(m *Monitor) { m.AcceptedStatus = []string{"299-200"} }, "accepted_status"},
		{"status out of range", func(m *Monitor) { m.AcceptedStatus = []string{"600"} }, "accepted_status"},
		{"no status codes", func(m *Monitor) { m.AcceptedStatus = nil }, "accepted_status"},
		{"duplicate tag", func(m *Monitor) { m.Tags = []string{"prod", "Prod"} }, "tags"},
		{"padded tag", func(m *Monitor) { m.Tags = []string{" prod"} }, "tags"},
		{"auth type", func(m *Monitor) { m.AuthType = "digest" }, "auth_type"},
	} {
		m := valid()
		c.edit(&m)
		if got := field(Validate(m, false)); got != c.field {
			t.Errorf("%s: field = %q, want %q", c.name, got, c.field)
		}
	}
}

func TestValidateURL(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		url             string
		public, private bool // accepted without / with allow_private_targets
	}{
		{"https://example.com/health?x=1", true, true},
		{"http://example.com:8080/", true, true},
		{"https://[2606:4700:4700::1111]/", true, true},
		{"https://user:pass@example.com/", false, false},
		{"https://user@example.com/", false, false},
		{"ftp://example.com/", false, false},
		{"file:///etc/passwd", false, false},
		{"gopher://example.com/", false, false},
		{"//example.com/", false, false},
		{"https:///path", false, false},
		{"https://example.com:0/", false, false},
		{"https://exa mple.com/", false, false},
		{"https://example.com/\n", false, false},
		{"http://localhost/", false, true},
		{"http://app.localhost/", false, true},
		{"http://127.0.0.1/", false, true},
		{"http://[::1]/", false, true},
		{"http://10.0.0.5/", false, true},
		{"http://192.168.1.1/", false, true},
		{"http://[::ffff:127.0.0.1]/", false, true},
		{"http://169.254.169.254/latest/meta-data/", false, false},
		{"http://[fd00:ec2::254]/", false, false},
		{"http://[fe80::1]/", false, false},
		{"http://0.0.0.0/", false, false},
		{"http://2130706433/", false, false},
		{"http://0177.0.0.1/", false, false},
		{"http://0x7f.1/", false, false},
		{"http://127.1/", false, false},
		{"http://" + strings.Repeat("a", 2050) + ".com/", false, false},
	} {
		if got := ValidateURL(c.url, false) == nil; got != c.public {
			t.Errorf("ValidateURL(%q, false) ok = %v, want %v", c.url, got, c.public)
		}
		if got := ValidateURL(c.url, true) == nil; got != c.private {
			t.Errorf("ValidateURL(%q, true) ok = %v, want %v", c.url, got, c.private)
		}
	}
}

func TestURLErrorsNeverEchoTheURL(t *testing.T) {
	t.Parallel()
	const token = "s3cr3t-token-value"
	for _, u := range []string{"https://u:" + token + "@example.com/", "ftp://example.com/?t=" + token, "http://127.0.0.1/?t=" + token} {
		err := ValidateURL(u, false)
		if err == nil || strings.Contains(err.Error(), token) {
			t.Errorf("ValidateURL(%q) = %v", u, err)
		}
	}
}

func TestHostAndRedactedURL(t *testing.T) {
	t.Parallel()
	const raw = "https://shop.example.com:8443/api/health?token=abc#frag"
	if got := Host(raw); got != "shop.example.com" {
		t.Errorf("Host = %q", got)
	}
	if got := RedactedURL(raw); got != "https://shop.example.com:8443/api/health" {
		t.Errorf("RedactedURL = %q", got)
	}
}

func TestStatusCodes(t *testing.T) {
	t.Parallel()
	codes, err := ParseStatusCodes([]string{"200-299", " 301 ", "418"})
	if err != nil {
		t.Fatal(err)
	}
	for code, want := range map[int]bool{199: false, 200: true, 299: true, 300: false, 301: true, 302: false, 418: true} {
		if codes.Accepts(code) != want {
			t.Errorf("Accepts(%d) = %v", code, !want)
		}
	}
	for _, bad := range [][]string{{"abc"}, {"99"}, {"200-"}, {"-200"}, {"300-200"}, {""}} {
		if _, err := ParseStatusCodes(bad); err == nil {
			t.Errorf("ParseStatusCodes(%q) accepted", bad)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestMergeSecret(t *testing.T) {
	t.Parallel()
	stored := Secret{Headers: map[string]string{"X-Api-Key": "old-key", "X-Other": "o"}, BearerToken: "tok", Body: "{}"}

	// Keep a header's value by name, replace another, drop the rest.
	got, names, err := MergeSecret(stored, SecretInput{HeadersSet: true, Headers: []HeaderInput{
		{Name: "x-api-key"}, {Name: "X-New", Value: ptr("n")},
	}}, AuthBearer, MethodPost)
	if err != nil {
		t.Fatal(err)
	}
	if got.Headers["X-Api-Key"] != "old-key" || got.Headers["X-New"] != "n" || len(got.Headers) != 2 {
		t.Errorf("headers = %v", got.Headers)
	}
	if strings.Join(names, ",") != "X-Api-Key,X-New" {
		t.Errorf("names = %v", names)
	}
	if got.BearerToken != "tok" || got.Body != "{}" {
		t.Error("nil inputs must keep the stored token and body")
	}

	// Switching to basic drops the bearer token and needs both credentials.
	if _, _, err := MergeSecret(stored, SecretInput{BasicUsername: ptr("u")}, AuthBasic, MethodPost); field(err) != "basic_password" {
		t.Errorf("basic without password: %v", err)
	}
	got, _, err = MergeSecret(stored, SecretInput{BasicUsername: ptr("u"), BasicPassword: ptr("p")}, AuthBasic, MethodPost)
	if err != nil || got.BearerToken != "" || got.BasicPassword != "p" {
		t.Errorf("basic = %+v, %v", got, err)
	}
	// None clears every credential; clearing the body with "".
	got, _, err = MergeSecret(stored, SecretInput{Body: ptr("")}, AuthNone, MethodGet)
	if err != nil || got.BearerToken != "" || got.Body != "" {
		t.Errorf("none = %+v, %v", got, err)
	}

	for _, c := range []struct {
		name string
		in   SecretInput
		auth AuthType
		m    Method
		f    string
	}{
		{"unknown kept header", SecretInput{HeadersSet: true, Headers: []HeaderInput{{Name: "X-Missing"}}}, AuthNone, MethodGet, "headers"},
		{"duplicate header", SecretInput{HeadersSet: true, Headers: []HeaderInput{{Name: "A", Value: ptr("1")}, {Name: "a", Value: ptr("2")}}}, AuthNone, MethodGet, "headers"},
		{"host header", SecretInput{HeadersSet: true, Headers: []HeaderInput{{Name: "Host", Value: ptr("evil")}}}, AuthNone, MethodGet, "headers"},
		{"proxy header", SecretInput{HeadersSet: true, Headers: []HeaderInput{{Name: "Proxy-Foo", Value: ptr("x")}}}, AuthNone, MethodGet, "headers"},
		{"header injection", SecretInput{HeadersSet: true, Headers: []HeaderInput{{Name: "X-A", Value: ptr("a\r\nX-B: b")}}}, AuthNone, MethodGet, "headers"},
		{"bad header name", SecretInput{HeadersSet: true, Headers: []HeaderInput{{Name: "X A", Value: ptr("a")}}}, AuthNone, MethodGet, "headers"},
		{"authorization with auth", SecretInput{HeadersSet: true, Headers: []HeaderInput{{Name: "Authorization", Value: ptr("x")}}}, AuthBearer, MethodGet, "headers"},
		{"empty bearer", SecretInput{BearerToken: ptr("")}, AuthBearer, MethodGet, "bearer_token"},
		{"body on GET", SecretInput{Body: ptr("x")}, AuthNone, MethodGet, "body"},
		{"body too large", SecretInput{Body: ptr(strings.Repeat("x", MaxRequestBodySize+1))}, AuthNone, MethodPost, "body"},
	} {
		if _, _, err := MergeSecret(Secret{}, c.in, c.auth, c.m); field(err) != c.f {
			t.Errorf("%s: field = %q (%v), want %q", c.name, field(err), err, c.f)
		}
	}
	// A custom Authorization header is fine without built-in auth.
	if _, _, err := MergeSecret(Secret{}, SecretInput{HeadersSet: true, Headers: []HeaderInput{{Name: "Authorization", Value: ptr("ApiKey x")}}}, AuthNone, MethodGet); err != nil {
		t.Errorf("custom Authorization: %v", err)
	}
}

func TestSecretNeverFormats(t *testing.T) {
	t.Parallel()
	const sentinel = "sentinel-7c1f"
	s := Secret{Headers: map[string]string{"X-K": sentinel}, BasicPassword: sentinel, BearerToken: sentinel, Body: sentinel}
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", slog.Any("secret", s), slog.Any("ptr", &s))
	js, _ := json.Marshal(map[string]any{"s": s})
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s", s, s, s, s), logged.String(), string(js)} {
		if strings.Contains(out, sentinel) {
			t.Errorf("secret leaked: %s", out)
		}
	}
	enc, err := EncodeSecret(s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeSecret(enc)
	if err != nil || back.Headers["X-K"] != sentinel || back.Body != sentinel {
		t.Errorf("round trip = %v", err)
	}
	if _, err := DecodeSecret([]byte("not json " + sentinel)); err == nil || strings.Contains(err.Error(), sentinel) {
		t.Errorf("DecodeSecret error = %v", err)
	}
}

func TestApply(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	m := valid() // retries 3, recovery 1
	up := Result{Up: true, StatusCode: 200}
	fail := Result{StatusCode: 503, Failure: FailureHTTPStatus}
	dnsFail := Result{Failure: FailureDNS}

	c := Counters{Status: StatusUnknown, StatusSince: t0}
	var beat Status
	var ev *Event
	// Three failures stay pending; the fourth is down.
	for i := 1; i <= 3; i++ {
		c, beat, ev = Apply(m, c, fail, false, t0.Add(time.Duration(i)*time.Minute))
		if c.Status != StatusPending || beat != StatusPending || ev != nil {
			t.Fatalf("failure %d: %s/%s/%v", i, c.Status, beat, ev)
		}
	}
	downAt := t0.Add(4 * time.Minute)
	c, beat, ev = Apply(m, c, fail, false, downAt)
	if c.Status != StatusDown || beat != StatusDown || ev == nil || ev.Kind != EventDown || *ev.StatusCode != 503 || ev.Failure != FailureHTTPStatus {
		t.Fatalf("fourth failure: %s/%s/%+v", c.Status, beat, ev)
	}
	if !c.StatusSince.Equal(downAt) || c.DownSince == nil || !c.DownSince.Equal(downAt) {
		t.Errorf("since = %v / %v", c.StatusSince, c.DownSince)
	}
	// Still down: no second event.
	if c, _, ev = Apply(m, c, fail, false, downAt.Add(time.Minute)); ev != nil || c.Status != StatusDown {
		t.Fatal("a second failure while down must not record an event")
	}
	// Probe offline: a network failure freezes everything.
	frozen := c
	if c2, b, e := Apply(m, c, dnsFail, true, downAt.Add(2*time.Minute)); c2 != frozen || b != StatusUnknown || e != nil {
		t.Errorf("offline network failure: %+v %s %v", c2, b, e)
	}
	// One success recovers, with the outage duration.
	upAt := downAt.Add(10 * time.Minute)
	c, beat, ev = Apply(m, c, up, false, upAt)
	if c.Status != StatusUp || beat != StatusUp || ev == nil || ev.Kind != EventUp || *ev.DurationSeconds != 600 || c.DownSince != nil {
		t.Fatalf("recovery: %s/%s/%+v", c.Status, beat, ev)
	}

	// retries 0: the first failure is down. recovery 2: one success is not enough.
	m.Retries, m.RecoverySuccesses = 0, 2
	c = Counters{Status: StatusUp, StatusSince: t0}
	if c, _, ev = Apply(m, c, fail, false, t0); c.Status != StatusDown || ev == nil {
		t.Fatal("retries 0 must go down on the first failure")
	}
	c, beat, ev = Apply(m, c, up, false, t0.Add(time.Minute))
	if c.Status != StatusDown || beat != StatusUp || ev != nil {
		t.Fatalf("first of two recovery successes: %s/%s/%v", c.Status, beat, ev)
	}
	if c, _, ev = Apply(m, c, up, false, t0.Add(2*time.Minute)); c.Status != StatusUp || ev == nil {
		t.Fatal("second success must recover")
	}
	// A pending monitor recovers on the first success, with no event.
	m.Retries = 2
	c, _, _ = Apply(m, Counters{Status: StatusUp}, fail, false, t0)
	if c, _, ev = Apply(m, c, up, false, t0); c.Status != StatusUp || ev != nil {
		t.Fatal("pending must recover silently")
	}
	// Offline does not hide a non-network failure.
	if c, _, _ = Apply(m, Counters{Status: StatusUp}, fail, true, t0); c.Status != StatusPending {
		t.Error("an http_status failure counts even while the probe is offline")
	}
}

func TestNextInterval(t *testing.T) {
	t.Parallel()
	m := valid()
	m.IntervalSeconds, m.RetryIntervalSeconds = 60, 20
	if NextInterval(m, Counters{Status: StatusPending}) != 20*time.Second || NextInterval(m, Counters{Status: StatusDown}) != time.Minute {
		t.Error("retry interval applies only while pending")
	}
}
