package armatureanalytics

import (
	"context"
	"os"

	"github.com/mark3labs/mcp-go/server"
)

// Shutdown flushes in-flight analytics batches. It is always safe to call,
// including when analytics is disabled — in that case it is a no-op. Pass
// a bounded context (e.g. context.WithTimeout) so a stuck ingest call
// cannot hold up process exit.
type Shutdown func(context.Context) error

// EnvConfig returns a Config populated from the standard environment
// variables: ANALYTICS_INGEST_API_KEY and ANALYTICS_INGEST_URL. When no API
// key is set, the returned Config produces a disabled (no-op) recorder.
//
// Callers that need additional fields (Timeout, OnError, …) should start
// from this value and override what they need:
//
//	cfg := armatureanalytics.EnvConfig()
//	cfg.OnError = func(err error, _ armatureanalytics.Batch) { … }
func EnvConfig() Config {
	return Config{
		APIKey:      os.Getenv("ANALYTICS_INGEST_API_KEY"),
		EndpointURL: os.Getenv("ANALYTICS_INGEST_URL"),
	}
}

// NewMCPServer constructs a *server.MCPServer with Armature analytics
// pre-wired from the environment. It is the one-line equivalent of:
//
//	rec, _   := armatureanalytics.NewRecorder(armatureanalytics.EnvConfig())
//	defer rec.Close(ctx)
//	s := server.NewMCPServer(name, version,
//	    append(opts, server.WithHooks(rec.Hooks()))...,
//	)
//
// The returned Shutdown flushes pending batches; defer it next to the
// constructor:
//
//	s, shutdown := armatureanalytics.NewMCPServer("my-mcp", "1.0.0",
//	    server.WithToolCapabilities(true),
//	)
//	defer func() {
//	    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	    defer cancel()
//	    _ = shutdown(ctx)
//	}()
//
// When ANALYTICS_INGEST_API_KEY is unset, no recorder is built, no hooks
// are added, and Shutdown is a no-op — so this call is safe to drop into
// a server that runs both with and without analytics enabled.
//
// For tools to surface intent/context in the dashboard, register them via
// armatureanalytics.InstrumentTool (instead of s.AddTool) so their input
// schema gets the optional telemetry block.
func NewMCPServer(name, version string, opts ...server.ServerOption) (*server.MCPServer, Shutdown) {
	return NewMCPServerWithConfig(name, version, EnvConfig(), opts...)
}

// NewMCPServerWithConfig is NewMCPServer with an explicit Config — use it
// when env-var-driven setup doesn't fit (e.g. callers with their own
// config layer, tests, or apps that want to set Config.OnError /
// Config.Timeout).
//
// Same semantics as NewMCPServer: cfg.APIKey == "" disables analytics
// cleanly; cfg.OnError is called both for ingest failures and for a
// failed recorder init (so callers see the error without panicking).
func NewMCPServerWithConfig(name, version string, cfg Config, opts ...server.ServerOption) (*server.MCPServer, Shutdown) {
	var rec *Recorder
	if cfg.APIKey != "" {
		r, err := NewRecorder(cfg)
		if err != nil {
			if cfg.OnError != nil {
				cfg.OnError(err, Batch{})
			}
			// Fall through with rec == nil so the server still runs.
		} else {
			rec = r
		}
	}
	if rec != nil {
		opts = append(opts, server.WithHooks(rec.Hooks()))
	}
	s := server.NewMCPServer(name, version, opts...)

	shutdown := Shutdown(func(ctx context.Context) error {
		if rec == nil {
			return nil
		}
		return rec.Close(ctx)
	})
	return s, shutdown
}
