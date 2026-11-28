package notification

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeMonitorChannels map[string][]string

func (f fakeMonitorChannels) MonitorChannelIDs(_ context.Context, monitorID string) ([]string, error) {
	return f[monitorID], nil
}

type fakeMonitorInfo map[string][2]string

func (f fakeMonitorInfo) MonitorInfo(_ context.Context, monitorID string) (string, string, error) {
	return f[monitorID][0], f[monitorID][1], nil
}

func monitorNotification(monitorID string) Notification {
	return Notification{EventID: "notif-website_down-m:" + monitorID + "--1-firing-critical", Kind: KindFiring,
		Trigger: TriggerWebsiteDown, MonitorID: monitorID, Severity: SeverityCritical, Title: "Website down",
		Message: "The website is down: the request timed out."}
}

func TestMonitorNotificationGoesOnlyToItsSelectedChannels(t *testing.T) {
	channels := newFakeChannelStore(testChannel("c1"), testChannel("c2"), testChannel("c3"))
	deliveries := newFakeDeliveryStore()
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries,
		MonitorChannels: fakeMonitorChannels{"picked": {"c2", "c3"}, "none": {}}})
	ctx := context.Background()
	for _, id := range []string{"picked", "none", "unknown"} {
		if err := e.Notify(ctx, monitorNotification(id)); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, d := range deliveries.all() {
		got = append(got, d.MonitorID+"/"+d.ChannelID)
	}
	slices.Sort(got)
	if want := []string{"picked/c2", "picked/c3"}; !slices.Equal(got, want) {
		t.Errorf("deliveries = %v, want %v", got, want)
	}

	// A node notification still reaches every enabled channel.
	if err := e.Notify(ctx, Notification{EventID: "n", Kind: KindFiring, Trigger: TriggerNodeDown, NodeID: "n1",
		Severity: SeverityCritical}); err != nil {
		t.Fatal(err)
	}
	if n := len(deliveries.all()); n != 5 {
		t.Errorf("deliveries after a node alert = %d, want 5", n)
	}
}

func TestMonitorNotificationWithoutAChannelSourceSendsNothing(t *testing.T) {
	deliveries := newFakeDeliveryStore()
	e := NewEngine(Options{Channels: newFakeChannelStore(testChannel("c1")), Deliveries: deliveries})
	if err := e.Notify(context.Background(), monitorNotification("m1")); err != nil {
		t.Fatal(err)
	}
	if n := len(deliveries.all()); n != 0 {
		t.Errorf("deliveries = %d, want none", n)
	}
}

func TestDispatchResolvesMonitorNameAndHostAtSendTime(t *testing.T) {
	deliveries := newFakeDeliveryStore()
	sender := &recordingSender{}
	e := NewEngine(Options{Channels: newFakeChannelStore(testChannel("c1")), Deliveries: deliveries,
		Senders:         map[ChannelType]Sender{ChannelTeams: sender},
		MonitorChannels: fakeMonitorChannels{"m1": {"c1"}},
		MonitorInfo:     fakeMonitorInfo{"m1": {"Shop", "shop.example.com"}}})
	ctx := context.Background()
	if err := e.Notify(ctx, monitorNotification("m1")); err != nil {
		t.Fatal(err)
	}
	e.DispatchOnce(ctx)
	if len(sender.sent) != 1 || sender.sent[0].MonitorName != "Shop" || sender.sent[0].MonitorHost != "shop.example.com" {
		t.Fatalf("sent = %+v", sender.sent)
	}
	if stored := deliveries.all()[0]; stored.MonitorName != "" || stored.MonitorHost != "" {
		t.Errorf("the name or host was stored: %+v", stored)
	}
}

func monitorDelivery() Delivery {
	down := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	return Delivery{EventID: "evt", Kind: KindRecovered, Trigger: TriggerWebsiteDown, MonitorID: "m1",
		MonitorName: "Shop", MonitorHost: "shop.example.com", Severity: SeverityCritical, Title: "Website down",
		Message: "The website is up again.", CreatedAt: down.Add(time.Hour),
		Details: MonitorDetails{StatusCode: ptrInt(200), ResponseMS: ptrInt(85), DownSince: &down, OutageSeconds: ptrInt(3720)}}
}

func TestTeamsCardForAMonitorHasNoNodeFields(t *testing.T) {
	t.Parallel()
	card := adaptiveCard(monitorDelivery())
	raw, _ := json.Marshal(card)
	body := string(raw)
	if !strings.Contains(body, "Recovered: Website down") {
		t.Errorf("title lacks the Recovered prefix: %s", body)
	}
	for _, want := range []string{`"Website"`, `"Shop"`, `"shop.example.com"`, `"200"`, `"85 ms"`, `"Down since"`, `"1 h 2 min"`} {
		if !strings.Contains(body, want) {
			t.Errorf("card lacks %s: %s", want, body)
		}
	}
	for _, unwanted := range []string{`"Node"`, `"Address"`, `"Subject"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("card has node field %s: %s", unwanted, body)
		}
	}
}

func TestTeamsCardForCertExpiryShowsDaysAndDate(t *testing.T) {
	t.Parallel()
	expires := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	d := Delivery{Kind: KindEscalated, Trigger: TriggerWebsiteCertExpiry, MonitorID: "m1", MonitorName: "Shop",
		Severity: SeverityCritical, Title: "Certificate expiring", Message: "m",
		Details: MonitorDetails{CertExpiresAt: &expires, CertDaysLeft: ptrInt(2)}}
	raw, _ := json.Marshal(adaptiveCard(d))
	for _, want := range []string{"Escalated: Certificate expiring", `"Days remaining"`, `"2"`, "2026-04-02 12:00 UTC"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("card lacks %s: %s", want, raw)
		}
	}
}

func TestWebhookMonitorPayload(t *testing.T) {
	t.Parallel()
	body := renderWebhook(t, `{"event": {{.eventJSON}}, "node": {{.nodeJSON}}, "monitor": {{.monitorJSON}}}`, monitorDelivery())
	var decoded struct {
		Event   map[string]any `json:"event"`
		Node    any            `json:"node"`
		Monitor map[string]any `json:"monitor"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	if decoded.Node != nil {
		t.Errorf("node = %v, want null for a monitor", decoded.Node)
	}
	if _, ok := decoded.Event["node_id"]; ok || decoded.Event["monitor_id"] != "m1" {
		t.Errorf("event = %v", decoded.Event)
	}
	want := map[string]any{"monitor_id": "m1", "name": "Shop", "host": "shop.example.com", "status_code": 200.0,
		"response_ms": 85.0, "down_since": "2026-03-01T10:00:00Z", "outage_seconds": 3720.0}
	for k, v := range want {
		if decoded.Monitor[k] != v {
			t.Errorf("monitor[%s] = %v, want %v (%s)", k, decoded.Monitor[k], v, body)
		}
	}
	if len(decoded.Monitor) != len(want) {
		t.Errorf("monitor has extra fields: %v", decoded.Monitor)
	}

	nodeBody := renderWebhook(t, `{{.monitorJSON}}`, Delivery{EventID: "e", Kind: KindFiring, NodeID: "n1", Title: "t", Message: "m"})
	if string(nodeBody) != "null" {
		t.Errorf("monitorJSON for a node alert = %s, want null", nodeBody)
	}
}
