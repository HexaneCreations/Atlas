package notification

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEngineSendFailsSafelyWithAnUnreadableSecret covers item 1d: a
// channel whose secret cannot be decrypted (missing key, wrong key,
// corrupt ciphertext — [corenotification.ErrSecretUnreadable] from
// Repository.ChannelSecret, simulated here) must fail that one delivery
// through the ordinary retry path, never panic, and never call Send at
// all — there is no plaintext to hand it.
func TestEngineSendFailsSafelyWithAnUnreadableSecret(t *testing.T) {
	channels := newFakeChannelStore(testChannel("c1"))
	channels.secretErr = fmt.Errorf("could not decrypt the channel secret: %w", ErrSecretUnreadable)
	deliveries := newFakeDeliveryStore()
	sender := &fakeSender{}
	e := NewEngine(Options{
		Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{ChannelTeams: sender},
		MaxAttempts: 3,
	})

	if err := e.Notify(context.Background(), Notification{EventID: "evt-1", Trigger: TriggerNodeDown}); err != nil {
		t.Fatalf("notify: %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("dispatchDue panicked on an unreadable secret: %v", r)
			}
		}()
		e.dispatchDue(context.Background())
	}()

	if sender.calls != 0 {
		t.Errorf("sender.calls = %d, want 0: Send must never run without a readable secret", sender.calls)
	}
	all := deliveries.all()
	if len(all) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(all))
	}
	if all[0].Status != StatusPending {
		t.Errorf("status = %q, want pending (retried, not exhausted — MaxAttempts is 3)", all[0].Status)
	}
	if all[0].Attempts != 1 {
		t.Errorf("attempts = %d, want 1", all[0].Attempts)
	}
	if !strings.Contains(all[0].LastError, "could not decrypt") {
		t.Errorf("last_error = %q, want it to say the secret could not be decrypted", all[0].LastError)
	}
}

// TestSecretNeverAppearsInLogsOrLastError covers item 1e's cross-path
// requirement directly, against the real Engine and a real TeamsSender —
// not just the Sender's own return value, but what the dispatch loop logs
// and what it writes to the delivery row, across a successful send, an
// HTTP-level failure, and a transport-level failure.
func TestSecretNeverAppearsInLogsOrLastError(t *testing.T) {
	const secretMarker = "s3cr3t-signature-must-never-leak-anywhere"

	for _, tc := range []struct {
		name   string
		client func(t *testing.T) (*http.Client, string) // returns the client and the destination URL
	}{
		{
			name: "success",
			client: func(t *testing.T) (*http.Client, string) {
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				t.Cleanup(srv.Close)
				return srv.Client(), srv.URL
			},
		},
		{
			name: "http error",
			client: func(t *testing.T) (*http.Client, string) {
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte("destination rejected the request"))
				}))
				t.Cleanup(srv.Close)
				return srv.Client(), srv.URL
			},
		},
		{
			name: "transport error",
			client: func(t *testing.T) (*http.Client, string) {
				// A server that is already closed: connecting to it fails
				// at the transport level, deterministically and without
				// depending on the real network.
				srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				url := srv.URL
				client := srv.Client()
				srv.Close()
				return client, url
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Not t.Parallel(): allowTestServerHost mutates
			// TeamsAllowedHostSuffixes.
			allowTestServerHost(t, "127.0.0.1")
			client, url := tc.client(t)

			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, nil))

			channels := newFakeChannelStore(testChannel("c1"))
			channels.secrets["c1"] = teamsSecretJSON(url + "?sig=" + secretMarker)
			deliveries := newFakeDeliveryStore()
			sender := &TeamsSender{client: client}
			e := NewEngine(Options{
				Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{ChannelTeams: sender},
				Logger: logger, MaxAttempts: 1,
			})

			if err := e.Notify(context.Background(), Notification{
				EventID: "evt-1", Trigger: TriggerNodeDown, Severity: SeverityCritical, Title: "t", Message: "m",
			}); err != nil {
				t.Fatalf("notify: %v", err)
			}
			e.dispatchDue(context.Background())

			all := deliveries.all()
			if len(all) != 1 {
				t.Fatalf("expected 1 delivery, got %d", len(all))
			}
			if strings.Contains(all[0].LastError, secretMarker) {
				t.Errorf("last_error leaked the secret: %q", all[0].LastError)
			}
			if strings.Contains(logBuf.String(), secretMarker) {
				t.Errorf("log output leaked the secret:\n%s", logBuf.String())
			}
		})
	}
}
