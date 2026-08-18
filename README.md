# Voice-to-Note

Go/Gin service that takes a voice memo, has an LLM turn it into a structured
note, and submits that note to an external tool.

```
POST /v1/notes (multipart audio)
        │
        ├─ 1. speech-to-text   (OpenAI-compatible /audio/transcriptions)
        ├─ 2. note extraction  (OpenAI-compatible /chat/completions, JSON mode)
        │      → { title, summary, action_items[] }
        └─ 3. submit           (webhook  → Zapier/Make/Sheets/CRM/webhook.site
                                notion   → one page per memo)
        │
        └─ 201 { note, sink, sink_ref }
```

## Quick start

Needs Go 1.26+ (`go` directive in `go.mod`). No database, no other services.

```bash
cp .env.example .env          # then edit it, see the table below
export $(grep -v '^#' .env | xargs)
make run                      # or: go run ./cmd/server
```

Fastest usable config: an OpenAI key plus a fresh URL from
[webhook.site](https://webhook.site) as `WEBHOOK_URL` — no third-party account
needed to see the note arrive.

```bash
curl -F "audio=@testdata/memo.m4a" http://localhost:8080/v1/notes
```

```json
{
  "data": {
    "note": {
      "title": "Invoice follow-up with Anna",
      "summary": "Anna has an unpaid invoice. Call her tomorrow and send the new offer.",
      "action_items": ["Call Anna tomorrow", "Send Anna the new offer"],
      "transcript": "Call Anna tomorrow about the unpaid invoice and send her the new offer."
    },
    "sink": "webhook",
    "sink_ref": "row-1"
  }
}
```

`testdata/memo.m4a` is the recording above. Record your own on macOS:
`say -o memo.aiff "..." && afconvert -f mp4f -d aac memo.aiff memo.m4a`

## API

| Method | Path | Notes |
|---|---|---|
| `GET` | `/healthz` | liveness probe |
| `POST` | `/v1/notes` | multipart form, file part **`audio`** |

Status codes: `201` created · `400` missing/invalid form · `413` audio over
`MAX_AUDIO_BYTES` · `415` unsupported extension · `422` no speech detected ·
`502` upstream (LLM or sink) failed. Errors use `{"error": "..."}`.

Every response carries an `X-Request-Id` header (inbound one is reused), and
that id appears on every log line for the request.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `LLM_API_KEY` | — | **required** |
| `LLM_BASE_URL` | `https://api.openai.com/v1` | any OpenAI-compatible API (e.g. Groq: `https://api.groq.com/openai/v1`) |
| `TRANSCRIBE_MODEL` | `whisper-1` | speech-to-text model |
| `CHAT_MODEL` | `gpt-4o-mini` | note extraction model |
| `SINK` | `webhook` | `webhook` or `notion` |
| `WEBHOOK_URL` | — | required for `SINK=webhook` |
| `NOTION_TOKEN`, `NOTION_PARENT_PAGE_ID` | — | required for `SINK=notion` |
| `ADDR` | `:8080` | listen address |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `MAX_AUDIO_BYTES` | `26214400` | 25 MiB, the speech-to-text API limit |
| `PROCESS_TIMEOUT` | `120s` | budget for transcribe + extract + submit |
| `SHUTDOWN_TIMEOUT` | `10s` | grace period for in-flight requests |

### Notion sink

Create an internal integration, share a page with it, and pass that page id as
`NOTION_PARENT_PAGE_ID`. Each memo becomes a page: title, summary paragraph,
action items as to-do blocks, transcript at the bottom. A *page* parent (rather
than a database) keeps the payload free of database-schema coupling.

## Logging

`log/slog` JSON on stdout, one access log line per request (method, path,
status, duration, size, client ip, request id) plus domain events. Nothing else
writes to stdout: the server uses `gin.New()` rather than `gin.Default()`, and
panics are recovered into a structured `error` line with the stack as a field.

Shipping to [Axiom](https://axiom.co) needs no code change: stdout is
newline-delimited JSON, so point Axiom's collector (or Vector, or the platform's
log driver) at the container's stdout and the fields land as-is.

Log levels map to status: `5xx` → error, `4xx` → warn, otherwise info, so an
Axiom alert on `level:error service:voice-to-note` is enough for basic monitoring.

## Tests

```bash
make test     # go test -race ./...
make cover    # coverage summary
```

Everything runs offline with no env vars: the domain flow uses fakes, the HTTP
layer uses `httptest` recorders, and the LLM/sink clients are pointed at
`httptest.Server` instances (both take an injectable base URL). CI
(`.github/workflows/ci.yml`) runs gofmt, `go vet`, build and `go test -race`
with coverage on every push and PR.

Coverage: 100% of the domain package, ~92% of the HTTP layer, 77% overall
(`cmd/server` wiring is not unit tested).

## Design notes / scope

* **Layout** — `internal/memo` owns the flow and defines
  `Transcriber`/`Analyzer`/`Sink`; `llm`, `sink` and `httpapi` are adapters. The
  interfaces exist because they carry two real implementations each (a fake in
  tests, plus two sinks), not for speculative extensibility.
* **Graceful shutdown** — `signal.NotifyContext` on SIGINT/SIGTERM, then
  `srv.Shutdown` within `SHUTDOWN_TIMEOUT`; a second Ctrl-C kills immediately.
* **Trust boundary** — `http.MaxBytesReader` cap, extension allowlist, upload
  filename is `filepath.Base`d before being forwarded, `ReadHeaderTimeout` set,
  and outbound calls inherit a per-request timeout context.
* **Streaming** — the upload is piped straight into the multipart request to the
  speech-to-text API, so a 25 MiB memo is not buffered twice.
* **One sink at a time** — `SINK` picks a single destination. Fan-out is a
  ~30-line composite that implements `memo.Sink` itself (`Save` loops, `Name`
  joins), so `memo`, `httpapi` and `cmd/server` would not change. The work is
  not the loop, it is the three decisions it forces: whether one failed
  delivery fails the whole request (a 5xx invites a retry that duplicates the
  page that *did* save), whether `Result.SinkRef` becomes `[]Delivery` and
  breaks the response contract, and sequential vs `errgroup`. Left as one sink
  until a second destination is actually wanted.
* **Deliberately left out** — no auth, queue, retries, rate limiting, database
  or Docker Compose. A production version would put step 2–3 on a worker queue
  and return `202` instead; for a 4-hour exercise the synchronous path is
  clearer.
* **Verified locally** — full request path against a stub upstream with a real
  `.m4a` file (201/400/413/415 + graceful shutdown on SIGTERM), plus a live
  round-trip against the real OpenAI and Notion APIs: `testdata/memo.m4a` →
  whisper-1 → gpt-4o-mini → a Notion page with the summary paragraph, one
  `to_do` per action item and the transcript.
