package startup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

	tlsCertFile := strings.TrimSpace(cfg.TLSCertFile)
	tlsKeyFile := strings.TrimSpace(cfg.TLSKeyFile)
	tlsClientCAFile := strings.TrimSpace(cfg.TLSClientCAFile)
	if tlsCertFile != "" && tlsKeyFile != "" && tlsClientCAFile != "" {
		tlsConfig, err := serverTLSConfigWithClientCA(cfg.Server.TLSConfig, tlsClientCAFile)
		if err != nil {
			return fmt.Errorf("startup tls: %w", err)
		}
		cfg.Server.TLSConfig = tlsConfig
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting service", "service", serviceName, "addr", addr, "version", strings.TrimSpace(cfg.Version))

		var err error
		switch {
		case tlsCertFile != "" && tlsKeyFile != "":
			logger.Info("tls enabled", "service", serviceName)
			if tlsClientCAFile != "" {
				logger.Info("verified client certificates required", "service", serviceName)
			}
			if cfg.Listener != nil {
				err = cfg.Server.ServeTLS(cfg.Listener, tlsCertFile, tlsKeyFile)
			} else {
				err = cfg.Server.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
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

func serverTLSConfigWithClientCA(base *tls.Config, clientCAFile string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read client ca file %q: %w", clientCAFile, err)
	}

	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse client ca file %q: no certificates found", clientCAFile)
	}

	if base == nil {
		base = &tls.Config{}
	} else {
		base = base.Clone()
	}
	base.ClientAuth = tls.RequireAndVerifyClientCert
	base.ClientCAs = clientCAs
	return base, nil
}
