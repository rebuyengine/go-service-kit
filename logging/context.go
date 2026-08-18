package logging

import (
	"context"
	"log/slog"
)

// ctxKey is unexported so no other package can collide with or forge these keys.
type ctxKey int

const requestIDKey ctxKey = iota

// WithRequestID returns a context carrying id.
//
// It lives in this package rather than in httpmw so that logging has no
// dependency on the HTTP layer, and so a background worker can attach a
// correlation id the same way a request can.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the id attached by WithRequestID, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)

	return id
}

// contextHandler copies the request id out of the context onto every record.
//
// Without it, correlation is manual: a handler's error line and the access line
// for the same request share nothing, and you are left matching on timestamps.
// With it, every line emitted anywhere beneath a request — including from code
// that knows nothing about HTTP — carries the same request_id.
//
// slog's own API is what makes this necessary: attributes are attached to a
// logger, not to a context, so a value that only exists per-request has no other
// route onto the record. The handler must therefore be told about the context,
// which is why callers should prefer the *Context methods (InfoContext,
// ErrorContext) — the plain ones pass context.Background and the id is lost.
type contextHandler struct{ inner slog.Handler }

func (h *contextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestIDFromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}

	return h.inner.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &contextHandler{inner: h.inner.WithAttrs(as)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{inner: h.inner.WithGroup(name)}
}
