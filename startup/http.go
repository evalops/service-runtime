package startup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/evalops/service-runtime/httpkit"
)

// HTTPServerConfig describes a managed HTTP service startup.
type HTTPServerConfig struct {
	ServiceName           string
	Addr                  string
	Version               string
	Environment           string
	Server                *http.Server
	Listener              net.Listener
	ShutdownTimeout       time.Duration
	Lifecycle             *Lifecycle
	Logger                *slog.Logger
	TLSCertFile           string
	TLSKeyFile            string
	TLSClientCAFile       string
	ConnectServiceNames   []string
	GRPCReflectionEnabled *bool
}

// NotifyContext returns a signal-aware root context for long-running services.
func NotifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// RunHTTPServer starts server and coordinates graceful shutdown with lifecycle hooks.
func RunHTTPServer(ctx context.Context, cfg HTTPServerConfig) error {
	if cfg.Server == nil {
		return fmt.Errorf("startup: server is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	timeout := cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = "service"
	}

	lifecycle := cfg.Lifecycle
	if lifecycle == nil {
		lifecycle = NewLifecycle()
	}
	if err := lifecycle.EnableTracingFromEnv(ctx, serviceName); err != nil {
		return fmt.Errorf("startup tracing: %w", err)
	}

	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		addr = cfg.Server.Addr
	}
	if addr == "" && cfg.Listener != nil {
		addr = cfg.Listener.Addr().String()
	}

	cfg.Server.Handler = httpkit.WithBrowserSecurityDefaults(nonNilHTTPHandler(cfg.Server.Handler))

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting service", "service", serviceName, "addr", addr, "version", strings.TrimSpace(cfg.Version))

		var err error
		switch {
		case strings.TrimSpace(cfg.TLSCertFile) != "" && strings.TrimSpace(cfg.TLSKeyFile) != "":
			logger.Info("tls enabled", "service", serviceName)
			if strings.TrimSpace(cfg.TLSClientCAFile) != "" {
				logger.Info("verified client certificates required", "service", serviceName)
			}
			if cfg.Listener != nil {
				err = cfg.Server.ServeTLS(cfg.Listener, "", "")
			} else {
				err = cfg.Server.ListenAndServeTLS("", "")
			}
		case cfg.Listener != nil:
			err = cfg.Server.Serve(cfg.Listener)
		default:
			err = cfg.Server.ListenAndServe()
		}

		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			return nil
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if lifecycleErr := lifecycle.Shutdown(shutdownCtx); lifecycleErr != nil {
			return errors.Join(fmt.Errorf("listen failed: %w", err), lifecycleErr)
		}
		return fmt.Errorf("listen failed: %w", err)
	case <-ctx.Done():
		logger.Info("shutting down service", "service", serviceName)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		var errs []error
		if err := cfg.Server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
			errs = append(errs, fmt.Errorf("shutdown server: %w", err))
		}
		if lifecycleErr := lifecycle.Shutdown(shutdownCtx); lifecycleErr != nil {
			errs = append(errs, lifecycleErr)
		}
		return errors.Join(errs...)
	}
}

func nonNilHTTPHandler(handler http.Handler) http.Handler {
	if handler == nil {
		return http.DefaultServeMux
	}
	return handler
}
