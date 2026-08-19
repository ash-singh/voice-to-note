# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make run                      # go run ./cmd/server (needs env, see below)
make test                     # go test -race ./...
make cover                    # coverage summary
make fmt vet tidy

go test ./internal/httpapi/                                   # one package
go test ./internal/sink/ -run TestNotionSaveCreatesPageUnderParent -v   # one test
go test -race ./internal/queue/                                # concurrent claiming
```

CI (`.github/workflows/ci.yml`) runs `gofmt -l .` (fails on any unformatted
file), `go vet`, `go build`, then `go test -race` with coverage. Run `make fmt`
before pushing.

`make run` needs at minimum `LLM_API_KEY` and `WEBHOOK_URL`; `config.Load`
fails fast with a joined error listing every missing variable. See
`.env.example` and the table in README.md.

## Architecture

`POST /v1/notes` (multipart field `audio`) spools the upload and returns `202`
with a job id; a worker then runs the pipeline: transcribe → extract a structured
note via an LLM → submit that note to an external tool. `GET /v1/notes/{id}`
reports the job (and the note once done), `GET /healthz` is the probe.

`internal/memo` is the core: it owns `Service.Process`, **defines** the
`Transcriber`, `Analyzer` and `Sink` interfaces it needs, and owns `SinkError`,
which is what lets a worker tell a failure that cannot have delivered from one
that might have. Everything else is an
adapter that satisfies them, so the dependency arrows point inward:

- `internal/llm` — one `Client` implements both `Transcriber` (multipart POST to
  `/audio/transcriptions`, streamed through `io.Pipe`) and `Analyzer`
  (`/chat/completions` in JSON mode, content unmarshalled into `memo.Note`).
  `BaseURL` is injectable, which is what makes any OpenAI-compatible provider
  work and what lets tests point at an `httptest.Server`.
- `internal/sink` — `Webhook` (JSON POST anywhere) and `Notion` (a page per memo
  under a *page* parent, so no database schema coupling). `sink.New(cfg)` picks
  one from config.
- `internal/httpapi` — Gin layer. Accepts and looks up work through the narrow
  `Jobs` interface, never `*queue.Queue`, and never touches `memo.Service`: the
  handler validates the upload and hands it over, nothing more.
- `internal/queue` — the durable job queue, and the reason the API is
  asynchronous. A directory per state (`tmp`, `pending`, `active`, `done`,
  `failed`); `os.Rename` provides both atomic publish and worker-exclusive
  claiming, so there is no lock anywhere. Job files are
  `<due>-<attempt>-<id><ext>`, the id being a content hash — which is what makes
  a re-upload idempotent. Retry policy hinges on `memo.SinkError`: only failures
  that cannot have reached the sink are retried; anything that might have
  delivered is dead lettered for review.
- `internal/config` — env → `Config`, validated in `Load`.

When adding a step or a delivery target, add the interface method or
implementation in the domain/adapter package; do not let `httpapi` or
`cmd/server` grow business logic. `cmd/server/main.go` is wiring only and is
deliberately not unit tested — which is why anything with behaviour belongs in a
package instead: `Recover` used to be called from `main`, and deleting that call
broke no test at all.

### Conventions that are load-bearing

- **The queue is single-instance.** `Queue.Run` calls `Recover` before starting
  workers, and it resolves everything in `active/` — which would steal another
  process's in-flight jobs if two servers shared a `QUEUE_DIR`. Scope `active/`
  per instance before running more than one.
- **`Lookup` sweeps the directories twice.** A job moves between them while it
  runs (a worker claims it, a retry puts it back) and each directory is a
  separate syscall, so one sweep can miss a job that moves across it and report
  a 404 for live work. Job state follows the job *file*, never the `.reason`
  sidecar, which is written just after the rename.
- **Tests run offline with zero env vars.** Domain tests use fakes; HTTP tests
  use `httptest` recorders; client tests use `httptest.Server` via the
  injectable base URL. Never make a test read a real key or reach the network,
  and reset ambient env in `internal/config` tests (see `clearEnv`).
- **Logging.** `logging.New` returns a JSON slog logger wrapped in a handler
  that copies the context request ID onto every record, so always use the
  `...Context` methods (`InfoContext`, `ErrorContext`) and thread `ctx` through.
  The router uses `gin.New()`, never `gin.Default()` — Gin's own logger would
  duplicate the structured access log written by `httpapi.RequestLogger`.
- **HTTP status mapping** lives in `httpapi.NoteHandler.Create`: 202 accepted,
  400 missing/invalid form, 413 over `MAX_AUDIO_BYTES`, 415 extension not in
  `allowedAudioExt`, 429 `queue.ErrQueueFull` (with `Retry-After`). `Show` adds
  404 for `queue.ErrJobNotFound`. Errors use `{"error": "..."}`, success uses
  `{"data": ...}`. Note what is *absent*: an unusable recording or a failed
  delivery happens after the response, so it surfaces as `state: "failed"` on
  the job, not as a 4xx/5xx. Do not reintroduce 422/502 on `Create`.
- Upstream failures wrap the cause (`fmt.Errorf("transcribe: %w", err)`) and
  error bodies from third parties are echoed only up to `maxErrBody` bytes. The
  sink step is the exception and returns a typed `*memo.SinkError` instead, on
  purpose: the queue routes on it with `errors.As`, so replacing it with a
  formatted string would silently turn dead lettering back into blind retries.

## Scope

This is a time-boxed coding challenge. It has a durable queue, retries with
backoff, dead lettering and a backlog cap; it deliberately has no auth, no
per-client rate limiting, no database and no Docker, and retries do not honour
upstream `Retry-After`. README's "Design notes / scope" is the current list.
Prefer keeping it that way over adding infrastructure.
