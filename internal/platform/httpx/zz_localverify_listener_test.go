package httpx_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// Runs the agent-facing middleware chain behind a real bound TCP listener
// and drives it with actual net/http client requests and a raw TCP
// connection — evidence for goal §11 that exercises the real wire path,
// not just an in-process handler call. Per-node rate limiting is proven
// separately in ratelimit_test.go, which needs a real mTLS peer
// certificate to key on; this test uses a plain listener and documents
// that unauthenticated requests here are correctly not rate limited.
func TestLocalVerifyListenerHardeningOverRealSocket(t *testing.T) {
	fleetCfg := config.Default().Fleet
	fleetCfg.MaxRequestBytes = 1024
	fleetCfg.RequestTimeout = 200 * time.Millisecond

	mux := http.NewServeMux()
	mux.HandleFunc("/valid", func(w http.ResponseWriter, r *http.Request) {
		// Mirrors the real telemetry handler: MaxBytesReader only errors
		// once something actually reads past the cap, exactly like
		// json.Decoder does against a real request body.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/panics", func(w http.ResponseWriter, r *http.Request) {
		panic("simulated handler panic")
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			w.WriteHeader(http.StatusGatewayTimeout)
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	})

	handler := httpx.AgentMiddleware(fleetCfg)(mux)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	plain := &http.Server{Handler: handler}
	go plain.Serve(ln)
	defer plain.Shutdown(context.Background())

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("valid request succeeds", func(t *testing.T) {
		resp, err := client.Get(base + "/valid")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("oversized body rejected", func(t *testing.T) {
		body := bytes.Repeat([]byte("x"), 4096)
		resp, err := client.Post(base+"/valid", "application/octet-stream", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		t.Logf("oversized body (4096 bytes, cap %d): status = %d", fleetCfg.MaxRequestBytes, resp.StatusCode)
		if resp.StatusCode == http.StatusOK {
			t.Error("oversized body was accepted")
		}
	})

	t.Run("handler panic recovered, listener stays usable", func(t *testing.T) {
		resp, err := client.Get(base + "/panics")
		if err != nil {
			t.Fatalf("GET (panic route): %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", resp.StatusCode)
		}

		resp2, err := client.Get(base + "/valid")
		if err != nil {
			t.Fatalf("GET after panic: %v", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Error("listener did not survive a handler panic")
		}
	})

	t.Run("slow handler is cut off by the request timeout", func(t *testing.T) {
		start := time.Now()
		resp, err := client.Get(base + "/slow")
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("GET (slow route): %v", err)
		}
		defer resp.Body.Close()
		t.Logf("slow handler (2s of work, %s timeout): returned in %s, status %d", fleetCfg.RequestTimeout, elapsed, resp.StatusCode)
		if elapsed > time.Second {
			t.Errorf("request took %s, want it cut off near the %s timeout", elapsed, fleetCfg.RequestTimeout)
		}
	})

	t.Run("unauthenticated request is not rate limited", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			resp, err := client.Get(base + "/valid")
			if err != nil {
				t.Fatalf("GET %d: %v", i, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("request %d: status = %d, want 200 — no client cert means PerNodeRateLimit does not apply here", i, resp.StatusCode)
			}
		}
		t.Log("per-node rate limiting itself is proven with real certificates in TestPerNodeRateLimitBoundsOneNode / TestPerNodeRateLimitIsolatesNodes (ratelimit_test.go)")
	})

	t.Run("malformed request line rejected before reaching any handler", func(t *testing.T) {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("NOT A VALID HTTP REQUEST LINE\r\n\r\n"))
		buf := make([]byte, 512)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _ := conn.Read(buf)
		got := string(buf[:n])
		t.Logf("raw response to malformed request line: %q", got)
		if !strings.Contains(got, "400") {
			t.Errorf("expected a 400-class response to a malformed request line, got %q", got)
		}
	})
}
