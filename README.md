# go-service-kit

Shared building blocks for Rebuy's Go services: structured logging that Google
Cloud classifies correctly, and HTTP middleware that logs through it.

**Standard library only.** Nothing here depends on a router or a logging
framework, so adopting it does not commit a service to either.

```go
import (
    "github.com/rebuyengine/go-service-kit/httpmw"
    "github.com/rebuyengine/go-service-kit/logging"
)

logger := logging.Setup(logging.Options{Service: "email-service", Version: Version})

srv := &http.Server{
    Addr:     ":8080",
    Handler:  handler,
    ErrorLog: logging.ErrorLog(logger), // 🔴 see below — do not skip this
}

r.Use(httpmw.RequestID)
r.Use(httpmw.Recoverer(logger))
r.Use(httpmw.RequestLogger(logger))
```

## The problem this solves

Google's logging agent decides an entry's severity from **the stream it was
written to** whenever the payload is not structured JSON carrying a severity it
recognises:

| stream | payload | resulting severity |
| --- | --- | --- |
| stdout | anything unstructured | `INFO` |
| **stderr** | **anything unstructured** | **`ERROR`** |
| either | JSON with a `level` field | mapped from `level` ✅ |

Nothing about the text matters. A library's routine debug chatter written to
stderr arrives stamped `ERROR`.

Measured across Rebuy's production cluster on 2026-08-18, three containers were
emitting plain-text lines whose own contents read INFO or DEBUG, and every one was
classified `ERROR` — each with an empty `jsonPayload`, which is the tell:

```
severity=ERROR  textPayload="[mysql] 2026/08/18 22:09:43 … write: broken pipe"
```

So this package writes **JSON to stdout, always**, and `Setup` additionally
redirects the standard library's `log` package — the usual escape hatch, since it
points at stderr and half the ecosystem reaches for it.

### 🔴 Always set `http.Server.ErrorLog`

Left nil, `net/http` logs through the standard logger to **stderr, as plain text**:
TLS handshake failures, malformed requests, connection resets. Every one arrives
as an unstructured `ERROR` from a service that is behaving perfectly. Point it at
`logging.ErrorLog(logger)` and those become structured `WARN` entries tagged
`event=http_server_error` — visible, filterable, and not mistaken for a fault.

### Why `httpmw` replaces chi's middleware instead of wrapping it

Both of chi's equivalents write to stderr:

- `middleware.Recoverer` writes the panic stack straight to `os.Stderr`
  (`var recovererErrorWriter io.Writer = os.Stderr`).
- `middleware.Logger` writes human-formatted text through a `log.Logger`.

Either way the output lands as unstructured `textPayload` — misclassified, and
useless as the basis for a log-based metric. A panic is exactly the thing you want
to alert on, so it must not be the thing that arrives as an unparseable blob.

## Alert on `event`, never on `severity`

`severity` is partly a function of which stream some dependency chose, which is
outside your control. Every line this module emits carries an explicit `event`
field; build log-based metrics and alert policies on **that**, and treat severity
as a hint for humans reading Logs Explorer.

| `event` | emitted by | meaning |
| --- | --- | --- |
| `http_request` | `httpmw.RequestLogger` | one per request |
| `http_panic` | `httpmw.Recoverer` | a handler panicked |
| `http_server_error` | `logging.ErrorLog` | net/http's own errors, usually client-side |
| `stdlib_log` | `logging.Setup` | something called `log.Print` |

These names are an API. Renaming one silently stops a metric matching, and the
alert built on it never fires again — a failure that shows up as a graph reading
zero. `httpmw` exports them as constants so a consuming service can assert on them.

## What is never logged

Request and response **bodies**, **headers**, and **query strings**. Rebuy's
payloads carry recipient email addresses, password-reset URLs and verification
codes; logging any of it ships PII and live credentials into a third-party sink
under someone else's retention policy.

Correlation is by `request_id` instead — attached by `httpmw.RequestID`, echoed on
the response as `X-Request-Id`, and added to **every** line emitted during that
request, including from code that knows nothing about HTTP.

> ⚠️ The id rides on the `context`, so only slog's `*Context` methods can see it.
> `logger.ErrorContext(ctx, …)` carries it; `logger.Error(…)` does not.

## Configuration

| Variable | Default | Effect |
| --- | --- | --- |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. An unrecognised value falls back to `info` rather than silencing the service |

## Testing

`Options.Output` exists so tests can capture output. **Production must leave it
nil** — that is what selects stdout.

## License

[MIT](LICENSE). Chosen over Apache 2.0 for simplicity — no `NOTICE` file to maintain and no
patent-grant or state-changes clauses to reason about — and because MIT is the dominant
license in the Go ecosystem, so it surprises nobody vendoring this.
