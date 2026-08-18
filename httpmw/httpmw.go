// Package httpmw provides HTTP middleware that logs through log/slog.
//
// It deliberately depends on nothing outside the standard library. A shared
// module inherited by every Go service should not drag a router choice along with
// it, and the pieces here are small enough that a dependency would cost more than
// it saves.
//
// It also replaces two pieces of chi middleware rather than wrapping them, for one
// reason: both write to stderr.
//
//   - chi's Recoverer writes the panic stack straight to os.Stderr
//     (middleware/recoverer.go: `var recovererErrorWriter io.Writer = os.Stderr`).
//   - chi's Logger writes human-formatted text through a log.Logger.
//
// Either way the output arrives in Cloud Logging as unstructured textPayload,
// which is both misclassified by severity and useless as the basis for a
// log-based metric. A panic is precisely the thing you want to alert on, so it
// cannot be the thing that arrives as an unparseable blob. See the logging
// package comment.
package httpmw

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/rebuyengine/go-service-kit/logging"
)

// Event values set on the "event" field. Alert policies filter on these.
//
// They are the contract between this module and the monitoring stack: renaming
// one silently stops an alert from ever firing, so treat them as an API.
const (
	EventRequest = "http_request"
	EventPanic   = "http_panic"
)

// RequestIDHeader is read from the request and echoed on the response, so a
// caller-supplied id survives into this service's logs and a generated one is
// visible to the caller.
const RequestIDHeader = "X-Request-Id"

// RequestID attaches a request id to the context and the response.
//
// An inbound X-Request-Id is trusted and reused so a trace spans services; the
// value is length-capped and otherwise treated as opaque. It is only ever a log
// field and a response header — never used to build a path, a query or an
// identity decision — so an attacker controlling it gains nothing beyond
// polluting their own log lines.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || len(id) > maxRequestIDLen {
			id = newRequestID()
		}

		w.Header().Set(RequestIDHeader, id)

		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

// maxRequestIDLen bounds a caller-supplied id so it cannot bloat every log line
// this request produces.
const maxRequestIDLen = 64

// newRequestID returns 16 random hex characters.
//
// crypto/rand rather than math/rand, not for unpredictability — this is only a
// correlation key — but because it needs no seeding and no shared mutable state,
// so it is safe from every goroutine without a lock. Errors are impossible in
// practice (rand.Read is documented to fill the buffer or die), and a partially
// filled id would still correlate, so the error is ignored deliberately.
func newRequestID() string {
	var b [8]byte

	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

// RequestLogger emits one structured line per request.
//
// 🔒 Bodies are never logged, and neither are headers. This service's payloads
// carry recipient email addresses, password-reset URLs and verification codes;
// putting any of that in a log ships PII and live credentials to a third-party
// sink and into whatever retention policy applies there. The request id is how a
// support question gets answered without storing any of it.
//
// The level tracks the status: 5xx logs at ERROR, everything else at INFO. That
// makes Logs Explorer readable for a human, but it is NOT what alerting keys on —
// policies filter on event and status, because severity can be influenced by
// libraries outside this code.
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}

			defer func() {
				level := slog.LevelInfo
				if rec.status >= http.StatusInternalServerError {
					level = slog.LevelError
				}

				logger.LogAttrs(r.Context(), level, "http request",
					slog.String("event", EventRequest),
					slog.String("method", r.Method),
					// r.URL.Path only: a query string would be caller-controlled
					// text copied verbatim into the log. No endpoint here takes
					// one, so there is nothing to lose by omitting it.
					slog.String("path", r.URL.Path),
					slog.Int("status", rec.status),
					slog.Int64("duration_ms", time.Since(start).Milliseconds()),
					slog.Int64("bytes_in", r.ContentLength),
					slog.Int("bytes_out", rec.written),
				)
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

// Recoverer turns a panic into a structured ERROR line and a 500.
//
// http.ErrAbortHandler is re-panicked rather than swallowed, matching net/http's
// contract: it is the documented way a handler says "drop this connection without
// a response", and logging it as a fault would be wrong.
//
// The stack is captured as a field rather than written to stderr, which is the
// whole reason this exists instead of chi's version.
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rvr := recover()
				if rvr == nil {
					return
				}

				if rvr == http.ErrAbortHandler {
					panic(rvr)
				}

				logger.LogAttrs(r.Context(), slog.LevelError, "handler panicked",
					slog.String("event", EventPanic),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Any("panic", rvr),
					slog.String("stack", stack()),
				)

				w.WriteHeader(http.StatusInternalServerError)
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// recorder captures the status and byte count without buffering the body.
type recorder struct {
	http.ResponseWriter
	status  int
	written int
	wrote   bool
}

func (r *recorder) WriteHeader(status int) {
	if r.wrote {
		return
	}

	r.status = status
	r.wrote = true

	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	// A handler that writes without calling WriteHeader has implicitly sent 200;
	// record that so the log does not claim a status the client never saw.
	if !r.wrote {
		r.wrote = true
	}

	n, err := r.ResponseWriter.Write(b)
	r.written += n

	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, so wrapping
// does not silently disable flushing or deadline control for a handler that
// needs them.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
