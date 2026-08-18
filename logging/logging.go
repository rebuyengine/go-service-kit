// Package logging configures log/slog for services that run in GKE.
//
// # Why this package exists at all
//
// Google's logging agent decides a log entry's severity from the STREAM it was
// written to whenever the payload is not structured JSON carrying a severity it
// recognises: stdout becomes INFO, stderr becomes ERROR. Nothing about the text
// matters — a library's routine debug chatter written to stderr arrives in Cloud
// Logging stamped ERROR.
//
// That is not hypothetical. Measured across Rebuy's production cluster on
// 2026-08-18, three containers were emitting plain-text lines whose own contents
// said INFO or DEBUG and every one of them was classified ERROR; each had an empty
// jsonPayload, which is the tell. When the payload IS structured JSON, the agent
// maps the level field to severity correctly.
//
// So the rule this package enforces is simple and absolute: everything goes to
// STDOUT, as JSON. Setup additionally redirects the standard library's logger,
// because that is the escape hatch — net/http, database drivers and assorted
// libraries all reach for it, and it points at stderr by default.
//
// # Do not alert on severity
//
// Severity is partly a function of which stream some dependency chose, which is
// outside your control. Alerting policies should filter on an explicit field your
// own code sets — an "event" key — and treat severity as a hint for humans reading
// Logs Explorer. See the README.
package logging

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
)

// Options configures the logger. The zero value is valid and produces a
// JSON logger at INFO on stdout.
type Options struct {
	// Level is the minimum level to emit. When nil, LevelFromEnv is used.
	Level slog.Leveler

	// Service is added to every line as "service", so entries stay attributable
	// when several services share a log sink.
	Service string

	// Version is added to every line as "version" when non-empty. Pass the build
	// stamp so a log line can be tied to the image that produced it.
	Version string

	// Output overrides the destination. Tests set this; production must not.
	// A nil Output means os.Stdout — see the package comment for why that is not
	// a preference.
	Output io.Writer
}

// New builds a logger without touching any global state.
//
// Prefer Setup in a main(); use New when you need an isolated logger, which in
// practice means tests.
func New(opts Options) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = os.Stdout
	}

	level := opts.Level
	if level == nil {
		level = LevelFromEnv()
	}

	handler := slog.Handler(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level}))
	handler = &contextHandler{inner: handler}

	logger := slog.New(handler)

	if opts.Service != "" {
		logger = logger.With("service", opts.Service)
	}

	if opts.Version != "" {
		logger = logger.With("version", opts.Version)
	}

	return logger
}

// Setup builds the logger AND claims the process's global logging state:
// slog's default logger, and — the part that actually matters — the standard
// library's log package.
//
// Redirecting log is not tidiness. log.Print writes to stderr, so any dependency
// using it produces unstructured stderr output that Cloud Logging stamps ERROR
// regardless of content. Routing it through slog turns those lines into structured
// stdout entries at a level you choose.
//
// It returns the logger for the caller to keep, since threading an explicit logger
// is better than reaching for the default everywhere.
func Setup(opts Options) *slog.Logger {
	logger := New(opts)

	slog.SetDefault(logger)

	// Anything still calling log.Print lands here: routed to slog at WARN, so it
	// is visible without being mistaken for an error.
	log.SetFlags(0)
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			strings.TrimRight(string(p), "\n"),
			slog.String("event", "stdlib_log"),
		)

		return len(p), nil
	}))

	return logger
}

// ErrorLog adapts a slog.Logger for http.Server.ErrorLog.
//
// 🔴 Set this on every http.Server. Left nil, net/http logs through the standard
// logger to STDERR as plain text — TLS handshake failures, malformed requests,
// connection resets — and every one of those arrives in Cloud Logging as an
// unstructured ERROR. A service that is behaving perfectly then appears to be
// emitting errors, and an alert policy keyed on severity pages someone for it.
//
// WARN rather than ERROR is deliberate: these are overwhelmingly client-side
// problems (a scanner, a dropped connection), not faults in the service.
func ErrorLog(logger *slog.Logger) *log.Logger {
	return slog.NewLogLogger(
		errorLogHandler{inner: logger.Handler()},
		slog.LevelWarn,
	)
}

// errorLogHandler stamps net/http's messages with an event key so they can be
// filtered and counted like anything else this service emits.
type errorLogHandler struct{ inner slog.Handler }

func (h errorLogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h errorLogHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(slog.String("event", "http_server_error"))

	return h.inner.Handle(ctx, r)
}

func (h errorLogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return errorLogHandler{inner: h.inner.WithAttrs(as)}
}

func (h errorLogHandler) WithGroup(name string) slog.Handler {
	return errorLogHandler{inner: h.inner.WithGroup(name)}
}

// LevelFromEnv reads LOG_LEVEL, defaulting to INFO.
//
// Accepts debug/info/warn/error in any case. An unrecognised value falls back to
// INFO rather than failing: a typo in a manifest should not silence a service or
// stop it booting.
func LevelFromEnv() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
