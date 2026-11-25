package notification

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

const (
	leakPath   = "T0LEAKYPATH/B0LEAKY"
	leakHeader = "leaky-header-value-7788"
)

func wantNoLeak(t *testing.T, where, s string) {
	t.Helper()
	for _, secret := range []string{leakPath, "T0LEAKYPATH", leakHeader, "X-Leaky-Name", "example.com"} {
		if strings.Contains(s, secret) {
			t.Errorf("%s leaks %q: %s", where, secret, s)
		}
	}
}

func logTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// A destination that echoes the request's path and headers in a shape Redact
// cannot recognise must not get them into the outcome, the delivery log or
// the server log.
func TestSendTestNeverRepeatsWhatTheDestinationSent(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot %s with %s", r.URL.Path, r.Header.Get("X-Leaky-Name"))
	}))
	defer srv.Close()

	channels := newFakeChannelStore(Channel{ID: "c1", Name: "c1", Type: ChannelWebhook, Config: map[string]any{"template": `{{.msg}}`}})
	channels.secrets["c1"] = webhookSecretJSON(t, srv.URL+"/services/"+leakPath, map[string]string{"X-Leaky-Name": leakHeader})
	deliveries := newFakeDeliveryStore()
	var logs bytes.Buffer
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries, Logger: logTo(&logs),
		Senders: map[ChannelType]Sender{ChannelWebhook: &WebhookSender{client: srv.Client()}}})

	outcome, err := e.SendTest(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Delivered || outcome.Error != "the destination returned status 500" {
		t.Fatalf("outcome = %+v", outcome)
	}
	wantNoLeak(t, "outcome", outcome.Error)
	wantNoLeak(t, "log", logs.String())
	all := deliveries.all()
	if len(all) != 1 || all[0].Status != StatusFailed {
		t.Fatalf("deliveries = %+v", all)
	}
	wantNoLeak(t, "last_error", all[0].LastError)
}

func TestSendErrorCategories(t *testing.T) {
	client := newGuardedClient(time.Second)
	_, unsafeErr := client.Get("https://127.0.0.1:1/services/" + leakPath)
	malformed := &SendError{msg: `malformed HTTP response "` + leakPath + `"`, category: transportCategory(errors.New("malformed"))}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"unsafe address":    {wrapSendError("deliver webhook", unsafeErr), categoryUnsafe},
		"timeout":           {wrapSendError("deliver webhook", context.DeadlineExceeded), categoryTimeout},
		"malformed":         {malformed, categoryRequest},
		"redirect":          {ErrRedirectRefused, ErrRedirectRefused.Error()},
		"unreadable secret": {errs.Wrap(ErrSecretUnreadable, errs.CodeFailedPrecondition, "x"), categorySecret},
		"no secret":         {errs.New(errs.CodeFailedPrecondition, "x"), categoryNoSecret},
		"invalid config":    {errs.New(errs.CodeInvalidArgument, "header %q", leakHeader), categoryConfig},
		"anything else":     {errors.New("boom " + leakPath), categoryUnspecified},
	} {
		if got := SendErrorCategory(c.err); got != c.want {
			t.Errorf("%s: category = %q, want %q", name, got, c.want)
		}
	}
}

func draftEngine(channels *fakeChannelStore, sender *fakeSender) (*Engine, *fakeDeliveryStore, *bytes.Buffer) {
	var logs bytes.Buffer
	deliveries := newFakeDeliveryStore()
	return NewEngine(Options{Channels: channels, Deliveries: deliveries, Logger: logTo(&logs),
		Senders: map[ChannelType]Sender{ChannelTeams: sender, ChannelWebhook: sender}}), deliveries, &logs
}

func draftInput(typ ChannelType, secret string) ChannelInput {
	in := ChannelInput{Name: "draft", Type: typ, MinSeverity: SeverityWarning}
	if typ == ChannelWebhook {
		in.Config = map[string]any{"template": `{{.msg}}`}
	}
	if secret != "" {
		in.Secret = &secret
	}
	return in
}

func TestDraftTestSendsAndStoresNothing(t *testing.T) {
	sender := &fakeSender{}
	e, deliveries, logs := draftEngine(newFakeChannelStore(), sender)
	d, err := e.PrepareDraftTest(context.Background(), draftInput(ChannelWebhook, webhookSecretJSON(t, "https://Hooks.Example.com/"+leakPath, nil)), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Target() != "host:hooks.example.com" {
		t.Errorf("target = %q", d.Target())
	}
	if out := d.Send(context.Background()); !out.Delivered || out.Error != "" {
		t.Fatalf("outcome = %+v", out)
	}
	sender.err = errors.New("boom " + leakPath)
	if out := d.Send(context.Background()); out.Delivered || out.Error != categoryUnspecified {
		t.Fatalf("failed outcome = %+v", out)
	}
	if len(deliveries.all()) != 0 || logs.Len() != 0 {
		t.Errorf("a draft test stored %d deliveries and logged %q", len(deliveries.all()), logs.String())
	}
}

func TestDraftTestUsesTheStoredSecretOnlyForItsOwnType(t *testing.T) {
	channels := newFakeChannelStore(Channel{ID: "c1", Name: "saved", Type: ChannelWebhook})
	channels.secrets["c1"] = webhookSecretJSON(t, "https://saved.example.com/"+leakPath, nil)
	e, _, _ := draftEngine(channels, &fakeSender{})
	ctx := context.Background()

	for _, secret := range []string{"", "  "} {
		d, err := e.PrepareDraftTest(ctx, draftInput(ChannelWebhook, secret), "c1")
		if err != nil {
			t.Fatalf("secret %q: %v", secret, err)
		}
		if d.Target() != "host:saved.example.com" || d.ChannelID != "c1" || d.secret != channels.secrets["c1"] {
			t.Errorf("secret %q: the stored secret was not used (target %q)", secret, d.Target())
		}
	}

	// A draft's own secret wins over channel_id.
	d, err := e.PrepareDraftTest(ctx, draftInput(ChannelWebhook, webhookSecretJSON(t, "https://other.example.com/x", nil)), "c1")
	if err != nil || d.Target() != "host:other.example.com" || d.ChannelID != "" {
		t.Errorf("draft secret: %v %+v", err, d)
	}

	// A Teams secret is a valid webhook secret too: only the type check
	// keeps it from being posted a webhook's body.
	channels.channels["c2"] = Channel{ID: "c2", Name: "teams", Type: ChannelTeams}
	channels.secrets["c2"] = teamsSecretJSON("https://prod-1.westus.logic.azure.com/workflows/x")
	if _, err := e.PrepareDraftTest(ctx, draftInput(ChannelWebhook, ""), "c2"); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Errorf("another type: err = %v, want invalid_argument", err)
	}

	channels.secretErr = errs.New(errs.CodeFailedPrecondition, "no secret")
	if _, err := e.PrepareDraftTest(ctx, draftInput(ChannelWebhook, ""), "c1"); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Errorf("no stored secret: err = %v, want invalid_argument", err)
	}
	channels.secretErr = errs.Wrap(ErrSecretUnreadable, errs.CodeFailedPrecondition, "wrong key")
	d, err = e.PrepareDraftTest(ctx, draftInput(ChannelWebhook, ""), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if out := d.Send(ctx); out.Delivered || out.Error != categorySecret {
		t.Errorf("unreadable secret: %+v", out)
	}
}

func TestDraftTestRejectsInvalidDraftsWithoutSendingOrLeaking(t *testing.T) {
	hook := func(url string) ChannelInput { return draftInput(ChannelWebhook, webhookSecretJSON(t, url, nil)) }
	cases := map[string]ChannelInput{
		"no secret":     draftInput(ChannelWebhook, ""),
		"not json":      draftInput(ChannelTeams, "https://hooks.example.com/"+leakPath),
		"http":          hook("http://hooks.example.com/" + leakPath),
		"userinfo":      hook("https://u:p@hooks.example.com/" + leakPath),
		"teams host":    draftInput(ChannelTeams, teamsSecretJSON("https://hooks.example.com/"+leakPath)),
		"teams suffix":  draftInput(ChannelTeams, teamsSecretJSON("https://evil-logic.azure.com.example.com/"+leakPath)),
		"denied header": draftInput(ChannelWebhook, webhookSecretJSON(t, "https://hooks.example.com/"+leakPath, map[string]string{"Cookie": leakHeader})),
		"header name":   draftInput(ChannelWebhook, webhookSecretJSON(t, "https://hooks.example.com/", map[string]string{"X-Leaky-Name\n": leakHeader})),
		"no template":   {Name: "d", Type: ChannelWebhook, MinSeverity: SeverityWarning, Secret: hook("https://hooks.example.com/").Secret},
		"no name":       {Type: ChannelTeams, MinSeverity: SeverityWarning},
		"unknown type":  {Name: "d", Type: "sms", MinSeverity: SeverityWarning},
	}
	for _, host := range []string{
		"127.0.0.1", "127.0.0.1.", "127.1", "2130706433", "0x7f000001", "0x7f.1", "0177.0.0.1", "localhost", "LOCALHOST.", "a.localhost",
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"[::1]", "[::]", "[fd00::1]", "[fe80::1%25eth0]", "[::ffff:127.0.0.1]", "[64:ff9b::7f00:1]",
	} {
		cases["host "+host] = hook("https://" + host + ":8443/" + leakPath)
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			sender := &fakeSender{}
			e, _, _ := draftEngine(newFakeChannelStore(), sender)
			_, err := e.PrepareDraftTest(context.Background(), in, "")
			if errs.CodeOf(err) != errs.CodeInvalidArgument {
				t.Fatalf("err = %v, want invalid_argument", err)
			}
			wantNoLeak(t, "error", err.Error())
			if sender.calls != 0 {
				t.Error("a rejected draft reached the sender")
			}
		})
	}
}

func TestDraftTestAcceptsPublicHosts(t *testing.T) {
	e, _, _ := draftEngine(newFakeChannelStore(), &fakeSender{})
	for _, in := range []ChannelInput{
		draftInput(ChannelTeams, teamsSecretJSON("https://prod-1.westus.logic.azure.com/workflows/x?sig=y")),
		draftInput(ChannelWebhook, webhookSecretJSON(t, "https://8.8.8.8/x", nil)),
		draftInput(ChannelWebhook, webhookSecretJSON(t, "https://[2606:4700::1]/x", nil)),
		draftInput(ChannelWebhook, webhookSecretJSON(t, "https://1password.example.com/x", nil)),
	} {
		if _, err := e.PrepareDraftTest(context.Background(), in, ""); err != nil {
			t.Errorf("%s: %v", *in.Secret, err)
		}
	}
	// No sender for email: accepted, and reported as a failed send.
	d, err := e.PrepareDraftTest(context.Background(), draftInput(ChannelEmail, "pw"), "")
	if err != nil {
		t.Fatal(err)
	}
	if out := d.Send(context.Background()); out.Delivered || !strings.Contains(out.Error, "no sender registered") {
		t.Errorf("email outcome = %+v", out)
	}
}

func TestTestLimiter(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewTestLimiter(clock.now)
	allow := func(user, target string) bool { _, ok := l.Allow(user, target); return ok }

	if !allow("u1", "a") || allow("u1", "b") {
		t.Fatal("a second test by one user inside 5s must be refused, whatever the target")
	}
	if allow("u2", "a") {
		t.Fatal("a second test of one target inside 5s must be refused, whoever asks")
	}
	if !allow("u2", "b") {
		t.Fatal("another user and target is unaffected")
	}
	clock.advance(3 * time.Second)
	if wait, ok := l.Allow("u1", "a"); ok || wait != 2*time.Second {
		t.Fatalf("wait = %v ok = %v, want 2s", wait, ok)
	}

	// Ten a minute: the first was at 0s.
	clock.advance(2 * time.Second)
	for i := range TestUserMax - 1 {
		if !allow("u1", fmt.Sprint("t", i)) {
			t.Fatalf("test %d refused", i+2)
		}
		clock.advance(TestMinGap)
	}
	// 50s in: the gap is satisfied, the minute is not.
	if wait, ok := l.Allow("u1", "z"); ok || wait != 10*time.Second {
		t.Fatalf("11th in a minute: wait = %v ok = %v, want 10s", wait, ok)
	}
	clock.advance(10 * time.Second)
	if !allow("u1", "z") {
		t.Fatal("refused after the window moved on")
	}
}

func TestTestLimiterDoesNotLeakKeys(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewTestLimiter(clock.now)
	for i := range 10_000 {
		l.Allow("u", fmt.Sprint("refused-", i)) // all but the first are refused
	}
	if len(l.byTarget) != 1 || len(l.byUser["u"]) != 1 {
		t.Fatalf("refused tests were recorded: %d targets, %d times", len(l.byTarget), len(l.byUser["u"]))
	}
	for i := range 10_000 {
		clock.advance(TestMinGap)
		l.Allow(fmt.Sprint("user-", i), fmt.Sprint("host-", i))
	}
	if len(l.byUser) > 13 || len(l.byTarget) > 2 {
		t.Errorf("holding %d users and %d targets", len(l.byUser), len(l.byTarget))
	}
	clock.advance(time.Hour)
	l.Allow("last", "last")
	if len(l.byUser) != 1 || len(l.byTarget) != 1 {
		t.Errorf("after an idle hour: %d users, %d targets", len(l.byUser), len(l.byTarget))
	}
}

func TestMuteUntil(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, want := range map[string]time.Time{
		"1h": now.Add(time.Hour), "4h": now.Add(4 * time.Hour), "24h": now.Add(24 * time.Hour),
		"7d": now.AddDate(0, 0, 7), "until_unmuted": MutedIndefinitely,
	} {
		if got, err := MuteUntil(name, now); err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
	for _, name := range []string{"", "0", "999d", "-1h", "1H", "2h", "1h "} {
		if _, err := MuteUntil(name, now); errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Errorf("%q: err = %v, want invalid_argument", name, err)
		}
	}
	// Serialisable in every zone a driver might scan it into.
	for _, offset := range []int{-12, 14} {
		if _, err := MutedIndefinitely.In(time.FixedZone("z", offset*3600)).MarshalJSON(); err != nil {
			t.Error(err)
		}
	}
}

// The sentinel is just a far-future time: decades on, the node is still
// muted, nothing is evaluated and the mute has not been ended.
func TestEvaluatorNeverEndsAnIndefiniteMute(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := MutedIndefinitely
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now, AlertsMutedUntil: &until}}, "")
	for range 5 {
		h.clock.advance(20 * 365 * 24 * time.Hour)
		h.eval.EvaluateOnce(context.Background())
	}
	if got := h.nodes.alertsMutedUntil("n1"); got == nil || !got.Equal(until) {
		t.Errorf("mute = %v, want %v", got, until)
	}
	if len(h.states.all()) != 0 || len(h.notifier.all()) != 0 {
		t.Error("a muted node was evaluated")
	}
}
