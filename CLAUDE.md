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
```

CI (`.github/workflows/ci.yml`) runs `gofmt -l .` (fails on any unformatted
file), `go vet`, `go build`, then `go test -race` with coverage. Run `make fmt`
before pushing.

`make run` needs at minimum `LLM_API_KEY` and `WEBHOOK_URL`; `config.Load`
fails fast with a joined error listing every missing variable. See
`.env.example` and the table in README.md.

## Architecture

One synchronous pipeline behind `POST /v1/notes` (multipart field `audio`):
transcribe → extract a structured note via an LLM → submit that note to an
external tool. `GET /healthz` is the probe.

`internal/memo` is the core: it owns `Service.Process` and **defines** the
`Transcriber`, `Analyzer` and `Sink` interfaces it needs. Everything else is an
adapter that satisfies them, so the dependency arrows point inward:

- `internal/llm` — one `Client` implements both `Transcriber` (multipart POST to
  `/audio/transcriptions`, streamed through `io.Pipe`) and `Analyzer`
  (`/chat/completions` in JSON mode, content unmarshalled into `memo.Note`).
  `BaseURL` is injectable, which is what makes any OpenAI-compatible provider
  work and what lets tests point at an `httptest.Server`.
- `internal/sink` — `Webhook` (JSON POST anywhere) and `Notion` (a page per memo
  under a *page* parent, so no database schema coupling). `sink.New(cfg)` picks
  one from config.
- `internal/httpapi` — Gin layer. Depends on the narrow `Processor` interface,
  not on `*memo.Service`.
- `internal/config` — env → `Config`, validated in `Load`.

When adding a step or a delivery target, add the interface method or
implementation in the domain/adapter package; do not let `httpapi` or
`cmd/server` grow business logic. `cmd/server/main.go` is wiring only and is
deliberately not unit tested.

### Conventions that are load-bearing

- **Tests run offline with zero env vars.** Domain tests use fakes; HTTP tests
  use `httptest` recorders; client tests use `httptest.Server` via the
  injectable base URL. Never make a test read a real key or reach the network,
  and reset ambient env in `internal/config` tests (see `clearEnv`).
- **Logging.** `logging.New` returns a JSON slog logger wrapped in a handler
  that copies the context request ID onto every record, so always use the
  `...Context` methods (`InfoContext`, `ErrorContext`) and thread `ctx` through.
  The router uses `gin.New()`, never `gin.Default()` — Gin's own logger would
  duplicate the structured access log written by `httpapi.RequestLogger`.
- **HTTP status mapping** lives in `httpapi.NoteHandler.Create`: 400
  missing/invalid form, 413 over `MAX_AUDIO_BYTES`, 415 extension not in
  `allowedAudioExt`, 422 `memo.ErrEmptyTranscript`, 502 any other pipeline
  error. Errors use `{"error": "..."}`, success uses `{"data": ...}`.
- Upstream failures wrap the cause (`fmt.Errorf("transcribe: %w", err)`) and
  error bodies from third parties are echoed only up to `maxErrBody` bytes.

## Scope

This is a time-boxed coding challenge (README documents the deliberate
omissions: no auth, retries, queue, rate limiting, Docker). Prefer keeping it
that way over adding infrastructure.
