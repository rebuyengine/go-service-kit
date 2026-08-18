package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/rebuyengine/go-service-kit/logging"
)

// decode returns the single JSON object written to buf.
func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("nothing was logged")
	}

	if i := strings.IndexByte(line, '\n'); i >= 0 {
		t.Fatalf("expected exactly one line, got:\n%s", line)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("output is not JSON (%v): %s", err, line)
	}

	return m
}

// TestOutputIsJSON is the load-bearing test in this package.
//
// The whole module exists because Google's logging agent classifies by stream
// when a payload is not structured JSON — so if output ever stops being parseable
// JSON, every consuming service silently loses correct severity AND every
// log-based metric built on a field stops matching. A human would not notice for
// weeks; this notices immediately.
func TestOutputIsJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logging.New(logging.Options{Output: &buf}).Info("hello", "k", "v")

	got := decode(t, &buf)

	if got["msg"] != "hello" || got["k"] != "v" {
		t.Errorf("unexpected payload: %v", got)
	}

	// The field the GKE agent maps to severity. Without it the entry inherits the
	// stream's default, which is the bug this module exists to prevent.
	if got["level"] != "INFO" {
		t.Errorf("level = %v, want INFO — the agent maps this to severity", got["level"])
	}
}

// TestServiceAndVersionAreOnEveryLine pins the attribution fields.
func TestServiceAndVersionAreOnEveryLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logging.New(logging.Options{Output: &buf, Service: "email-service", Version: "1.2.3"}).Info("x")

	got := decode(t, &buf)

	if got["service"] != "email-service" || got["version"] != "1.2.3" {
		t.Errorf("service/version missing: %v", got)
	}
}

// TestRequestIDReachesTheRecord covers the correlation guarantee.
//
// Also pins the trap in slog's design: the id rides on the CONTEXT, so only the
// *Context methods can see it. A caller using plain Info silently loses
// correlation, and that difference must be visible in a test rather than
// discovered while reading an incident's logs.
func TestRequestIDReachesTheRecord(t *testing.T) {
	t.Parallel()

	t.Run("with context", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		ctx := logging.WithRequestID(context.Background(), "abc123")
		logging.New(logging.Options{Output: &buf}).InfoContext(ctx, "x")

		if got := decode(t, &buf)["request_id"]; got != "abc123" {
			t.Errorf("request_id = %v, want abc123", got)
		}
	})

	t.Run("without context the field is absent, not empty", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		logging.New(logging.Options{Output: &buf}).Info("x")

		if _, present := decode(t, &buf)["request_id"]; present {
			t.Error("request_id present with no id in context; an empty value would pollute every non-request line")
		}
	})
}

// TestRequestIDRoundTripsThroughContext covers the accessor pair directly.
func TestRequestIDRoundTripsThroughContext(t *testing.T) {
	t.Parallel()

	if got := logging.RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("empty context yielded %q, want empty", got)
	}

	ctx := logging.WithRequestID(context.Background(), "xyz")
	if got := logging.RequestIDFromContext(ctx); got != "xyz" {
		t.Errorf("got %q, want xyz", got)
	}
}

// TestErrorLogIsStructuredAndNotAnError covers the net/http trap.
//
// Left nil, http.Server.ErrorLog writes plain text to STDERR, which Cloud Logging
// stamps ERROR — so TLS scans and client disconnects would page whoever is on
// call. This asserts both halves of the fix: the output is structured, and it is
// WARN rather than ERROR, because these are overwhelmingly client-side problems.
func TestErrorLogIsStructuredAndNotAnError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	errLog := logging.ErrorLog(logging.New(logging.Options{Output: &buf}))
	errLog.Print("http: TLS handshake error from 1.2.3.4:5678: EOF")

	got := decode(t, &buf)

	if got["level"] != "WARN" {
		t.Errorf("level = %v, want WARN — net/http noise must not read as a service fault", got["level"])
	}

	if got["event"] != "http_server_error" {
		t.Errorf("event = %v, want http_server_error", got["event"])
	}

	if msg, _ := got["msg"].(string); !strings.Contains(msg, "TLS handshake error") {
		t.Errorf("message not preserved: %v", got["msg"])
	}
}

// TestErrorLogSatisfiesHTTPServer is a compile-time-ish guard: the whole point is
// that this value can be assigned to http.Server.ErrorLog.
func TestErrorLogSatisfiesHTTPServer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	srv := &http.Server{ErrorLog: logging.ErrorLog(logging.New(logging.Options{Output: &buf}))}
	if srv.ErrorLog == nil {
		t.Fatal("ErrorLog did not produce a *log.Logger")
	}
}

// TestSetupCapturesTheStandardLogger covers the other stderr escape hatch.
//
// Database drivers and assorted libraries call log.Print, which goes to stderr and
// is therefore auto-classified ERROR. Setup must redirect it. The test restores
// the global afterwards so it cannot leak into other tests.
func TestSetupCapturesTheStandardLogger(t *testing.T) {
	// Not parallel: it mutates process-global logging state.
	origFlags := log.Flags()
	origOut := log.Writer()

	t.Cleanup(func() {
		log.SetFlags(origFlags)
		log.SetOutput(origOut)
	})

	var buf bytes.Buffer

	logging.Setup(logging.Options{Output: &buf})
	log.Print("legacy library chatter")

	got := decode(t, &buf)

	if got["event"] != "stdlib_log" {
		t.Errorf("event = %v, want stdlib_log", got["event"])
	}

	if got["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", got["level"])
	}

	if msg, _ := got["msg"].(string); msg != "legacy library chatter" {
		t.Errorf("msg = %q, want the original text with no trailing newline", msg)
	}
}

// TestLevelFromEnv covers parsing, including that a typo does not silence a
// service.
func TestLevelFromEnv(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		// A manifest typo must fall back to INFO rather than dropping output or
		// failing the process.
		"verbose": slog.LevelInfo,
	}

	for value, want := range cases {
		t.Setenv("LOG_LEVEL", value)

		if got := logging.LevelFromEnv(); got != want {
			t.Errorf("LOG_LEVEL=%q gave %v, want %v", value, got, want)
		}
	}
}

// TestLevelIsHonoured proves the level actually filters, not merely parses.
func TestLevelIsHonoured(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logging.New(logging.Options{Output: &buf, Level: slog.LevelWarn}).Info("suppressed")

	if buf.Len() != 0 {
		t.Errorf("INFO was emitted at WARN level: %s", buf.String())
	}
}

// TestDefaultOutputIsStdoutNotStderr is the most important test in this module.
//
// Every other test injects Options.Output, so none of them exercises the DEFAULT
// destination — and the default is the entire point of the package. Changing
// os.Stdout to os.Stderr in New() passed the whole suite before this existed,
// which would have shipped precisely the misclassification bug the package was
// written to prevent: every INFO line arriving in Cloud Logging stamped ERROR.
//
// It swaps the real file descriptors for pipes, so it asserts where bytes
// actually go rather than trusting a constant. Not parallel: os.Stdout and
// os.Stderr are process-global.
func TestDefaultOutputIsStdoutNotStderr(t *testing.T) {
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	t.Cleanup(func() { os.Stdout, os.Stderr = origOut, origErr })

	// No Output set: this is the production path.
	logging.New(logging.Options{}).Info("routed to the right stream")

	// Close the write ends before reading so ReadAll sees EOF instead of blocking.
	_ = outW.Close()
	_ = errW.Close()

	stdout, err := io.ReadAll(outR)
	if err != nil {
		t.Fatal(err)
	}

	stderr, err := io.ReadAll(errR)
	if err != nil {
		t.Fatal(err)
	}

	if len(stderr) != 0 {
		t.Errorf("wrote to STDERR, which GKE auto-classifies as ERROR: %s", stderr)
	}

	if !strings.Contains(string(stdout), "routed to the right stream") {
		t.Errorf("nothing reached stdout; got %q", stdout)
	}
}
