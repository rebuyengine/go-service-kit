package httpmw_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rebuyengine/go-service-kit/httpmw"
	"github.com/rebuyengine/go-service-kit/logging"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("nothing was logged")
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("output is not JSON (%v): %s", err, line)
	}

	return m
}

// serve runs one request through the full middleware chain and returns the
// recorder plus whatever was logged.
func serve(t *testing.T, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer

	logger := logging.New(logging.Options{Output: &buf})
	chain := httpmw.RequestID(httpmw.Recoverer(logger)(httpmw.RequestLogger(logger)(h)))

	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	return rec, &buf
}

func ok(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }

// TestRequestLogSchema pins every field an alert policy or dashboard depends on.
//
// These names are a contract with the monitoring stack: a log-based metric filters
// on `event`, and a renamed field means the metric silently matches nothing and the
// alert never fires again. That failure is invisible — the graph just reads zero —
// so it has to be caught here.
func TestRequestLogSchema(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/invite", strings.NewReader(`{"a":1}`))
	_, buf := serve(t, http.HandlerFunc(ok), req)

	got := decode(t, buf)

	for field, want := range map[string]any{
		"event":  httpmw.EventRequest,
		"method": http.MethodPost,
		"path":   "/api/v1/invite",
		"status": float64(http.StatusOK),
		"level":  "INFO",
	} {
		if got[field] != want {
			t.Errorf("%s = %v, want %v", field, got[field], want)
		}
	}

	for _, field := range []string{"duration_ms", "bytes_in", "bytes_out", "request_id"} {
		if _, present := got[field]; !present {
			t.Errorf("%s missing from the request log", field)
		}
	}

	if got["bytes_out"] != float64(len(`{"ok":true}`)) {
		t.Errorf("bytes_out = %v, want %d", got["bytes_out"], len(`{"ok":true}`))
	}
}

// TestNoBodyOrHeaderIsEverLogged is a privacy guard, not a formatting check.
//
// These payloads carry recipient addresses, reset URLs and verification codes.
// Logging any of it ships PII and live credentials into a third-party sink under
// someone else's retention policy. The assertion is deliberately blunt: the secret
// values must not appear ANYWHERE in the output.
func TestNoBodyOrHeaderIsEverLogged(t *testing.T) {
	t.Parallel()

	const (
		secretEmail = "victim@example.com"
		secretCode  = "424242"
		secretAuth  = "Bearer sk-live-do-not-log"
	)

	body := `{"email":"` + secretEmail + `","verificationCode":"` + secretCode + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/registration", strings.NewReader(body))
	req.Header.Set("Authorization", secretAuth)

	_, buf := serve(t, http.HandlerFunc(ok), req)

	for _, secret := range []string{secretEmail, secretCode, secretAuth, "sk-live"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("%q leaked into the log: %s", secret, buf.String())
		}
	}
}

// TestQueryStringIsNotLogged covers the other caller-controlled surface.
func TestQueryStringIsNotLogged(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health?token=secret-value", nil)
	_, buf := serve(t, http.HandlerFunc(ok), req)

	if strings.Contains(buf.String(), "secret-value") {
		t.Errorf("query string leaked into the log: %s", buf.String())
	}

	if got := decode(t, buf)["path"]; got != "/api/v1/health" {
		t.Errorf("path = %v, want the bare path", got)
	}
}

// TestServerErrorLogsAtErrorLevel keeps Logs Explorer readable for humans.
func TestServerErrorLogsAtErrorLevel(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, buf := serve(t, h, httptest.NewRequest(http.MethodPost, "/api/v1/invite", nil))

	got := decode(t, buf)

	if got["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR for a 5xx", got["level"])
	}

	if got["status"] != float64(http.StatusInternalServerError) {
		t.Errorf("status = %v, want 500", got["status"])
	}
}

// TestClientErrorIsNotAnError separates "the caller sent nonsense" from "we broke".
//
// If 4xx logged at ERROR, a caller sending malformed JSON would page whoever is on
// call, and the paging policy is that anything user-facing pages.
func TestClientErrorIsNotAnError(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	})

	_, buf := serve(t, h, httptest.NewRequest(http.MethodPost, "/api/v1/invite", nil))

	if got := decode(t, buf)["level"]; got != "INFO" {
		t.Errorf("level = %v, want INFO for a 4xx", got)
	}
}

// TestImplicitStatusIsRecordedAsOK covers a handler that writes without calling
// WriteHeader — otherwise the log would report a status the client never saw.
func TestImplicitStatusIsRecordedAsOK(t *testing.T) {
	t.Parallel()

	_, buf := serve(t, http.HandlerFunc(ok), httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := decode(t, buf)["status"]; got != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", got)
	}
}

// TestRequestIDIsGeneratedEchoedAndCorrelated covers all three jobs of the id.
func TestRequestIDIsGeneratedEchoedAndCorrelated(t *testing.T) {
	t.Parallel()

	t.Run("generated when absent", func(t *testing.T) {
		t.Parallel()

		rec, buf := serve(t, http.HandlerFunc(ok), httptest.NewRequest(http.MethodGet, "/x", nil))

		header := rec.Header().Get(httpmw.RequestIDHeader)
		if header == "" {
			t.Fatal("no request id echoed on the response")
		}

		if got := decode(t, buf)["request_id"]; got != header {
			t.Errorf("logged id %v does not match the echoed header %q", got, header)
		}
	})

	t.Run("an inbound id is reused so a trace spans services", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set(httpmw.RequestIDHeader, "upstream-id-1")

		rec, buf := serve(t, http.HandlerFunc(ok), req)

		if got := decode(t, buf)["request_id"]; got != "upstream-id-1" {
			t.Errorf("request_id = %v, want the inbound id", got)
		}

		if got := rec.Header().Get(httpmw.RequestIDHeader); got != "upstream-id-1" {
			t.Errorf("echoed %q, want the inbound id", got)
		}
	})

	t.Run("an absurd inbound id is replaced, not propagated", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set(httpmw.RequestIDHeader, strings.Repeat("A", 5000))

		_, buf := serve(t, http.HandlerFunc(ok), req)

		id, _ := decode(t, buf)["request_id"].(string)
		if len(id) > 64 {
			t.Errorf("oversized caller id survived (%d chars); every line of this request would carry it", len(id))
		}
	})

	t.Run("ids differ between requests", func(t *testing.T) {
		t.Parallel()

		_, a := serve(t, http.HandlerFunc(ok), httptest.NewRequest(http.MethodGet, "/x", nil))
		_, b := serve(t, http.HandlerFunc(ok), httptest.NewRequest(http.MethodGet, "/x", nil))

		if decode(t, a)["request_id"] == decode(t, b)["request_id"] {
			t.Error("two requests shared a request id; correlation would be meaningless")
		}
	})
}

// TestPanicIsStructuredNotStderr is the reason this package replaces chi's
// Recoverer rather than wrapping it.
//
// chi writes the stack to os.Stderr as raw text, which Cloud Logging stores as an
// unstructured entry — unusable as the basis for a log-based metric. A panic is
// exactly the thing that must page someone, so it cannot be the thing that arrives
// as an unparseable blob.
func TestPanicIsStructuredNotStderr(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	rec, buf := serve(t, h, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}

	// Two lines: the panic and the request log. Find the panic one.
	var panicLine map[string]any

	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("a line was not JSON — this is the chi behaviour we replaced: %s", l)
		}

		if m["event"] == httpmw.EventPanic {
			panicLine = m
		}
	}

	if panicLine == nil {
		t.Fatalf("no %s line emitted: %s", httpmw.EventPanic, buf.String())
	}

	if panicLine["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", panicLine["level"])
	}

	if panicLine["panic"] != "boom" {
		t.Errorf("panic = %v, want boom", panicLine["panic"])
	}

	if s, _ := panicLine["stack"].(string); !strings.Contains(s, "httpmw_test") {
		t.Error("stack was not captured as a field")
	}
}

// TestAbortHandlerIsNotSwallowed matches net/http's documented contract: it is how
// a handler says "drop this connection", and treating it as a fault would be wrong.
func TestAbortHandlerIsNotSwallowed(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := logging.New(logging.Options{Output: &buf})
	h := httpmw.Recoverer(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rvr := recover(); rvr != http.ErrAbortHandler {
			t.Errorf("recovered %v, want ErrAbortHandler to propagate", rvr)
		}
	}()

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	t.Error("ErrAbortHandler was swallowed")
}

// TestResponseControllerStillWorks proves the wrapper did not silently disable
// flushing for a handler that streams.
func TestResponseControllerStillWorks(t *testing.T) {
	t.Parallel()

	var flushed bool

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("chunk"))

		if err := http.NewResponseController(w).Flush(); err == nil {
			flushed = true
		}
	})

	serve(t, h, httptest.NewRequest(http.MethodGet, "/x", nil))

	if !flushed {
		t.Error("Flush failed through the recorder; Unwrap is not working")
	}
}
