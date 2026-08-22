package httpx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// TLSServer is a supervised HTTPS server with an explicit [tls.Config].
//
// Separate from [Server] because the agent-facing HTTPS listener needs
// client-certificate verification the browser-facing API must not have —
// see docs/architecture/agent-design.md §3. Two [lifecycle.Component]
// instances with different TLS policy is simpler and safer than one server
// whose TLS behavior branches by route.
//
// A nil tlsConfig serves the same lifecycle without terminating TLS at all,
// for a listener that is already an authenticated, encrypted transport — see
// [NewServerFromListener].
type TLSServer struct {
	name      string
	addr      string
	handler   http.Handler
	tlsConfig *tls.Config
	logger    *slog.Logger

	mu               sync.RWMutex
	srv              *http.Server
	listener         net.Listener
	preboundListener net.Listener
	faults           chan error
}

// NewTLSServer builds a server. It binds nothing until Start.
func NewTLSServer(name, addr string, handler http.Handler, tlsConfig *tls.Config, logger *slog.Logger) *TLSServer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &TLSServer{
		name: name, addr: addr, handler: handler, tlsConfig: tlsConfig,
		logger: logger, faults: make(chan error, 1),
	}
}

// NewTLSServerFromListener builds a server around an already-open listener
// instead of binding "tcp" itself — the seam a non-TCP transport plugs into
// without duplicating any of TLSServer's serve/shutdown/fault-reporting
// logic. TLS termination, the handler and every lifecycle behaviour are
// identical to [NewTLSServer]. A listener that already authenticates and
// encrypts its own connections wants [NewServerFromListener] instead.
func NewTLSServerFromListener(name string, listener net.Listener, handler http.Handler, tlsConfig *tls.Config, logger *slog.Logger) *TLSServer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &TLSServer{
		name: name, handler: handler, tlsConfig: tlsConfig,
		logger: logger, faults: make(chan error, 1), preboundListener: listener,
	}
}

// NewServerFromListener builds a server around an already-open listener that
// is *already* an authenticated, encrypted transport, so no TLS is
// terminated on top of it.
//
// The one such listener Atlas has is libp2p's: its Noise handshake has
// already authenticated both peers by Peer ID and encrypted the stream
// before a byte of HTTP is exchanged (see
// docs/adr/0012-connect-by-identity.md). Running TLS inside that would be a
// second handshake proving a second identity, which is precisely the
// enrollment dependency ADR-0012 exists to remove — not additional security.
// Every lifecycle behaviour is otherwise identical to
// [NewTLSServerFromListener]; only the handshake is absent.
func NewServerFromListener(name string, listener net.Listener, handler http.Handler, logger *slog.Logger) *TLSServer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &TLSServer{
		name: name, handler: handler, tlsConfig: nil,
		logger: logger, faults: make(chan error, 1), preboundListener: listener,
	}
}

// Name implements [lifecycle.Component].
func (s *TLSServer) Name() string { return s.name }

// Start binds the listener (or adopts the prebound one) and serves in the
// background.
func (s *TLSServer) Start(ctx context.Context) error {
	// Cloned rather than shared: ServeTLS's first call on a *tls.Config
	// mutates it in place (net/http's onceSetNextProtoDefaults ->
	// http2ConfigureServer adds ALPN protocols, sets GetCertificate, etc.).
	// Callers such as fleetPipeline hand the same *tls.Config to two
	// TLSServer instances (HTTPS and libp2p listeners); without a clone,
	// their concurrent first requests race on that shared struct.
	var clonedTLS *tls.Config
	if s.tlsConfig != nil {
		clonedTLS = s.tlsConfig.Clone()
	}
	srv := &http.Server{
		Handler:           s.handler,
		TLSConfig:         clonedTLS,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
		BaseContext: func(net.Listener) context.Context {
			return context.WithoutCancel(ctx)
		},
	}

	listener := s.preboundListener
	if listener == nil {
		lc := net.ListenConfig{}
		l, err := lc.Listen(ctx, "tcp", s.addr)
		if err != nil {
			return fmt.Errorf("bind %s: %w", s.addr, err)
		}
		listener = l
	}

	s.mu.Lock()
	s.srv = srv
	s.listener = listener
	s.mu.Unlock()

	s.logger.InfoContext(ctx, "server listening",
		slog.String("name", s.name), slog.String("addr", listener.Addr().String()),
		slog.Bool("tls", clonedTLS != nil))

	go func() {
		// TLSConfig already carries the server certificate, so ServeTLS
		// needs no cert/key file paths — see the net/http doc on Server.TLSConfig.
		// A nil config means the listener is already an authenticated,
		// encrypted transport (see NewServerFromListener) and Serve is used
		// unchanged.
		serve := func() error { return srv.ServeTLS(listener, "", "") }
		if clonedTLS == nil {
			serve = func() error { return srv.Serve(listener) }
		}
		if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.faults <- err
			return
		}
		close(s.faults)
	}()

	return nil
}

// Stop gracefully drains in-flight requests.
func (s *TLSServer) Stop(ctx context.Context) error {
	s.mu.RLock()
	srv := s.srv
	s.mu.RUnlock()
	if srv == nil {
		return nil
	}
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}

// Faults reports a listener that died at runtime.
func (s *TLSServer) Faults() <-chan error { return s.faults }

// Addr returns the bound address, useful when the configured port is 0.
func (s *TLSServer) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}
