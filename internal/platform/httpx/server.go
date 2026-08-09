package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/lifecycle"
)

// Server is a supervised HTTP server.
//
// It satisfies [lifecycle.Component] and [lifecycle.FaultReporter]: Start
// binds the listener and returns, Stop drains, and a listener that dies at
// runtime is reported as a fault so the supervisor can shut the process down
// in order rather than leaving a live process with no API.
type Server struct {
	cfg     config.Server
	handler http.Handler
	logger  *slog.Logger

	// mu guards srv and listener. Start runs on the supervisor's goroutine
	// while Addr may be called from any other — a health endpoint, a test
	// waiting for the bound port — so the handoff has to be synchronised.
	mu       sync.RWMutex
	srv      *http.Server
	listener net.Listener
	faults   chan error
}

// NewServer builds a server from configuration. It binds nothing until Start.
func NewServer(cfg config.Server, handler http.Handler, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		cfg:     cfg,
		handler: handler,
		logger:  logger,
		faults:  make(chan error, 1),
	}
}

// Name implements [lifecycle.Component].
func (s *Server) Name() string { return "http.server" }

// Start binds the listener and serves in the background.
//
// Binding happens synchronously so that a port conflict is a startup error —
// reported before the supervisor declares Atlas ready — rather than an
// asynchronous failure discovered after the process claims to be up.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		// Route net/http's own errors into the structured logger instead of
		// the standard logger, so they carry the same shape as everything
		// else and are not lost to stderr.
		ErrorLog: slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
		// Hand every connection a context carrying the request-scoped log
		// attributes established at startup.
		BaseContext: func(net.Listener) context.Context {
			return context.WithoutCancel(ctx)
		},
	}

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", s.cfg.Addr())
	if err != nil {
		return fmt.Errorf("bind %s: %w", s.cfg.Addr(), err)
	}

	s.mu.Lock()
	s.srv = srv
	s.listener = listener
	s.mu.Unlock()

	s.logger.InfoContext(ctx, "http server listening",
		slog.String("addr", listener.Addr().String()))

	go func() {
		// ErrServerClosed is the expected result of Shutdown, not a fault.
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.faults <- err
			return
		}
		close(s.faults)
	}()

	return nil
}

// Stop gracefully drains in-flight requests, then closes idle connections.
//
// It honours the context deadline the supervisor supplies; when that expires
// with requests still running, the connections are closed outright and the
// deadline error is returned so the shutdown is recorded as unclean rather
// than silently truncated.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.RLock()
	srv := s.srv
	s.mu.RUnlock()

	if srv == nil {
		return nil // never started
	}
	err := srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		s.logger.WarnContext(ctx, "graceful shutdown timed out, closing connections")
		return errors.Join(err, srv.Close())
	}
	return err
}

// Faults implements [lifecycle.FaultReporter].
func (s *Server) Faults() <-chan error { return s.faults }

// Addr returns the bound address, which is only meaningful after Start.
//
// It reports the address actually bound, so a test or a deployment that
// configures port 0 can discover the port the kernel assigned.
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.listener == nil {
		return s.cfg.Addr()
	}
	return s.listener.Addr().String()
}

// Compile-time proof that Server satisfies the lifecycle contracts. If either
// interface changes, this fails at build time rather than at 3am.
var (
	_ lifecycle.Component     = (*Server)(nil)
	_ lifecycle.FaultReporter = (*Server)(nil)
)

// BaseMiddleware returns the standard chain every Atlas HTTP route runs
// through, outermost first.
//
// The order is load-bearing:
//
//  1. Recoverer is outermost, so it catches panics in everything below it,
//     including the logger.
//  2. RequestID comes next, so every log line and error body below it —
//     including a recovered panic — carries the correlation id.
//  3. AccessLog wraps the rest, so its measured duration and observed status
//     include everything the request actually did.
//  4. SecurityHeaders and CORS run before the handler so headers are set
//     before any handler starts writing.
//  5. MaxBodyBytes and Timeout are innermost, closest to the handler whose
//     resource use they bound.
//
// Recoverer above RequestID means a panic in RequestID itself is still caught,
// at the cost of that one response having no correlation id — the right trade,
// since the alternative is an unrecovered panic.
func BaseMiddleware(cfg config.Server, requestTimeout time.Duration) Middleware {
	return Chain(
		Recoverer(),
		RequestID(),
		AccessLog(),
		SecurityHeaders(),
		CORS(cfg.AllowedOrigins),
		MaxBodyBytes(cfg.MaxRequestBytes),
		Timeout(requestTimeout),
	)
}
