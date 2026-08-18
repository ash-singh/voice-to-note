# Voice-to-Note

Go/Gin service. Takes a voice memo, an LLM turns it into a structured note, the
note goes to an external tool.

```
POST /v1/notes (multipart audio)
        │
        ├─ spooled to disk, id = hash of the audio
        └─ 202 { job_id, state }          ← caller is done here

worker (durable queue on disk)
        │
        ├─ 1. speech-to-text   (OpenAI-compatible /audio/transcriptions)
        ├─ 2. note extraction  (OpenAI-compatible /chat/completions, JSON mode)
        │      → { title, summary, action_items[] }
        └─ 3. submit           (webhook  → Zapier/Make/Sheets/CRM/webhook.site
                                notion   → one page per memo)

GET /v1/notes/{job_id} → queued | processing | done { note, sink, sink_ref } | failed { reason }
```

## Quick start

Needs Go 1.26+ (`go` directive in `go.mod`). No database. No other services.

```bash
cp .env.example .env          # then edit it, see table below
export $(grep -v '^#' .env | xargs)
make run                      # or: go run ./cmd/server
```

Fastest usable config: an OpenAI key plus a fresh URL from
[webhook.site](https://webhook.site) as `WEBHOOK_URL`. No third-party account
needed to see the note arrive.

```bash
curl -F "audio=@testdata/memo.m4a" http://localhost:8080/v1/notes
# {"data":{"job_id":"4730a7a45987cf68","state":"queued"}}

curl http://localhost:8080/v1/notes/4730a7a45987cf68
```

```json
{
  "data": {
    "id": "4730a7a45987cf68",
    "state": "done",
    "result": {
      "note": {
        "title": "Call Anna about Invoice and Offer",
        "summary": "Discuss the unpaid invoice with Anna and send her the new offer.",
        "action_items": ["Call Anna about the unpaid invoice", "Send Anna the new offer"],
        "transcript": "Call Anna tomorrow about the unpaid invoice and send her the new offer."
      },
      "sink": "webhook",
      "sink_ref": "https://webhook.site/<your-uuid>"
    }
  }
}
```

Poll until `state` is `done`. Before that: `queued` or `processing`. A `failed`
job carries a `reason` instead of a `result`.

`testdata/memo.m4a` is the recording above. That `job_id` is the id you get for
it — the id is a hash of the audio. Record your own on macOS:
`say -o memo.aiff "..." && afconvert -f mp4f -d aac memo.aiff memo.m4a`

## API

| Method | Path | Notes |
|---|---|---|
| `GET` | `/healthz` | liveness probe |
| `POST` | `/v1/notes` | multipart form, file part **`audio`** → `202` + `job_id` |
| `GET` | `/v1/notes/{job_id}` | job state, with the note once it is `done` |

`POST` status codes: `202` accepted · `400` missing/invalid form · `413` audio
over `MAX_AUDIO_BYTES` · `415` unsupported extension · `429` backlog at
`QUEUE_MAX_DEPTH`, with `Retry-After`. `GET` returns `200`, or `404` for an
unknown job. Errors use `{"error": "..."}`.

The work happens after the response. So an unusable recording or a failed
delivery is a property of the job, not of the request. Both show as
`state: "failed"` with a `reason`, not as a `4xx`/`5xx`.

The `job_id` is a hash of the audio. Re-posting the same recording returns the
same job and never delivers it twice.

Every response carries an `X-Request-Id` header (an inbound one is reused). That
id appears on every log line for the request.

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
| `PROCESS_TIMEOUT` | `120s` | budget for one job: transcribe + extract + submit |
| `SHUTDOWN_TIMEOUT` | `10s` | grace period for in-flight requests |
| `QUEUE_DIR` | `queue` | where spooled audio and job records live |
| `QUEUE_WORKERS` | `2` | concurrent jobs; size it to your LLM rate limit |
| `QUEUE_MAX_DEPTH` | `100` | backlog cap; also bounds disk at depth × `MAX_AUDIO_BYTES` |

### Notion sink

1. Create an internal integration.
2. Open the parent page in Notion.
3. Use `•••` → **Connections** → **Connect to** to give the integration access.

Without that grant the API returns `404` for the page, which looks exactly like a
wrong id. `NOTION_PARENT_PAGE_ID` is the bare 32-hex id at the end of the page
URL, without the title slug.

Each memo becomes a page: title, summary paragraph, action items as to-do
blocks, transcript at the bottom. A *page* parent (not a database) keeps the
payload free of database-schema coupling.

### Queue on disk

`QUEUE_DIR` is inspectable with `ls`. That is most of its appeal:

```
queue/
  tmp/       partial uploads; never seen by a worker
  pending/   <due>-<attempt>-<id>.m4a   waiting, oldest due first
  active/    claimed by a worker
  done/      <id>.json                 the Result, served by GET /v1/notes/{id}
  failed/    the audio + <id>.reason    dead letters, for a human
```

To replay a dead letter after you fix the cause:

```bash
mv queue/failed/1787062647-0-c7b3317585c25805.m4a queue/pending/
```

The claim clears the stale `.reason`. `done/` is never swept automatically. A
long-running deployment wants a cron job for that.

## Logging

`log/slog` JSON on stdout. One access log line per request (method, path,
status, duration, size, client ip, request id), plus domain events.

Worker lines carry `request_id: "job-<id>"` instead. The request that submitted
the job returns long before the work runs, so the job id is what correlates them.

Nothing else writes to stdout: the server uses `gin.New()`, not `gin.Default()`,
and panics are recovered into a structured `error` line with the stack as a field.

Log levels map to status: `5xx` → error, `4xx` → warn, else info. So an Axiom
alert on `level:error service:voice-to-note` is enough for basic monitoring.

Shipping to [Axiom](https://axiom.co) needs no code change. Stdout is
newline-delimited JSON. Point Axiom's collector (or Vector, or the platform's log
driver) at the container's stdout and the fields land as-is.

## Tests

```bash
make test     # go test -race ./...
make cover    # coverage summary
```

Everything runs offline with no env vars:

* domain flow uses fakes
* HTTP layer uses `httptest` recorders
* LLM and sink clients point at `httptest.Server` instances (both take an
  injectable base URL)

CI (`.github/workflows/ci.yml`) runs gofmt, `go vet`, build and `go test -race`
with coverage on every push and PR.

Coverage: 100% of the domain package, ~90% of the HTTP layer, ~79% of the queue,
77% overall (`cmd/server` wiring is not unit tested).

Queue tests use `t.TempDir()` and a fake processor. They drive one job at a time
through an exported `ProcessNext` instead of waiting on the worker loop, so
nothing sleeps and nothing touches the network. Concurrent claiming is covered
under `-race`. Two tests — partial uploads staying invisible, and the extension
surviving the rename to a job id — assert invariants the design already had. Each
was validated by mutating the implementation and watching it fail, not by
trusting a test that passed first time.

## Design notes / scope

* **Layout** — `internal/memo` owns the flow and defines
  `Transcriber`/`Analyzer`/`Sink`. `llm`, `sink` and `httpapi` are adapters. The
  interfaces exist because they carry two real implementations each (a fake in
  tests, plus two sinks), not for speculative extensibility.
* **Graceful shutdown** — `signal.NotifyContext` on SIGINT/SIGTERM, then
  `srv.Shutdown` within `SHUTDOWN_TIMEOUT`. A second Ctrl-C kills immediately.
* **Trust boundary** — `http.MaxBytesReader` cap, extension allowlist, upload
  filename is `filepath.Base`d before it is forwarded, `ReadHeaderTimeout` set,
  outbound calls inherit a per-request timeout context.
* **Streaming** — the upload is piped straight into the multipart request to the
  speech-to-text API. A 25 MiB memo is not buffered twice.
* **One sink at a time** — `SINK` picks a single destination. Fan-out is a
  ~30-line composite that implements `memo.Sink` itself (`Save` loops, `Name`
  joins), so `memo`, `httpapi` and `cmd/server` would not change. The work is not
  the loop. It is the two decisions the loop forces: does one failed delivery
  dead letter the whole job, and does `Result.SinkRef` become `[]Delivery` and
  break the response contract. Left as one sink until a second destination is
  actually wanted.
* **The queue is the directory** — `internal/queue`. An upload is spooled to
  `tmp/` and renamed into `pending/`, so a partial upload is never visible. A
  worker claims a job by winning the rename into `active/`. That is the entire
  locking story: no lock file, no lease, and a racing worker simply gets
  `ENOENT`. Job files are named `<due>-<attempt>-<id><ext>`, so the due time
  sorts first and a retry is a rename with a later due time, not a scheduler.
  Survives restart. Needs no database.
* **Idempotency** — the job id is a SHA-256 prefix of the audio, hashed during
  the same copy that spools it (`io.MultiWriter`, no extra pass). Re-posting a
  recording that is queued, in flight or already done returns the same job and
  does no new work. A client that retries a `202` cannot produce a second Notion
  page.
* **Which failures may be retried** — `memo.SinkError` makes this decidable.
  Transcription and analysis failures never reached the sink, so they retry with
  exponential backoff (30s, three attempts). A *delivery* failure may have stored
  the note before it failed, so it is dead lettered instead. So is a job
  interrupted by a restart with no recorded result. A visible `failed` is
  preferred over a possible duplicate — deliberate choice. `failed/` keeps the
  audio and a `.reason` file, so a replay after the fix is
  `mv queue/failed/<file>.m4a queue/pending/`; the claim clears the stale reason.
* **Backpressure** — `QUEUE_MAX_DEPTH` is checked *before* the upload is spooled,
  so a flood is refused without a disk write, and the cap doubles as the disk
  ceiling. A full queue answers `429` with `Retry-After`, not a generic `500`,
  because it is the one failure a client can act on.
* **Deliberately left out** — no auth, no per-client rate limiting, no database,
  no Docker Compose. Retries do not honour upstream `Retry-After` yet. The queue
  assumes a single instance owns its directory (`active/` recovery would race
  between two processes that share one).
* **Verified locally** — against the real OpenAI, Notion and webhook endpoints,
  not only stubs. `testdata/memo.m4a` → whisper-1 → gpt-4o-mini → a Notion page
  with the summary paragraph, one `to_do` per action item and the transcript, and
  the same note delivered as JSON to a webhook. Asynchronously: `202` →
  `processing` → `done` with the result; the same file re-posted returns the same
  `job_id` with no second delivery; a sink that returns `500` lands in `failed`
  with a reason that names it; an unreachable LLM reschedules the job for a later
  attempt. Plus graceful shutdown on SIGTERM.
