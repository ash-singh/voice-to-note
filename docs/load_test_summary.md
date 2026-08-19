# Load test summary

Load-tests the API/queue/worker path in isolation: `LLM_BASE_URL` and
`WEBHOOK_URL` point at a local mock (`tools/loadtest/mockapi`) instead of a
real LLM or sink, so results measure the app's own overhead, not third-party
latency or cost. Reproduce with `make loadtest` or `.github/workflows/load-test.yml`
(`workflow_dispatch`).

## Setup

- Server built from `cmd/server`, run against a scratch `QUEUE_DIR`.
- `tools/loadtest/mockapi` simulates realistic upstream latency: 50 ms
  transcribe, 100 ms chat completion, 20 ms webhook (~170 ms per job).
- `tools/loadtest/gen` posts unique 2 KiB fake-audio payloads at a given
  concurrency and count, polls each job to completion, reports submit and
  end-to-end latency percentiles.
- `QUEUE_MAX_DEPTH=100` (default) in every run.

## Results

### `QUEUE_WORKERS=2` (default)

| Concurrency | Requests | Accepted (202) | 429 | Errors | Throughput | Submit p99 | E2E p50 | E2E p99 |
|---|---|---|---|---|---|---|---|---|
| 5 | 50 | 50 | 0 | 0 | 10.7 req/s | 8.3 ms | 358 ms | 1060 ms |
| 20 | 200 | 200 | 0 | 0 | 10.7 req/s | 18.2 ms | 1777 ms | 2665 ms |
| 50 | 500 | 500 | 0 | 0 | 11.0 req/s | 47.8 ms | 4430 ms | 5304 ms |
| 150 (burst) | 400 | 118 | 282 | 0 | 33.0 req/s | 297.6 ms | 6877 ms | 11940 ms |

### `QUEUE_WORKERS=8`

| Concurrency | Requests | Accepted (202) | 429 | Errors | Throughput | Submit p99 | E2E p50 | E2E p99 |
|---|---|---|---|---|---|---|---|---|
| 5 | 50 | 50 | 0 | 0 | 9.9 req/s | 6.3 ms | 358 ms | 1153 ms |
| 20 | 200 | 200 | 0 | 0 | 37.2 req/s | 23.1 ms | 358 ms | 1365 ms |
| 50 | 500 | 500 | 0 | 0 | 41.2 req/s | 57.2 ms | 1147 ms | 2004 ms |
| 150 (burst) | 400 | 127 | 273 | 0 | 96.8 req/s | 367.7 ms | 2691 ms | 4085 ms |

"Throughput" is client-observed requests/sec over the whole run (submit +
drain), not raw ingest rate — ingest itself is sub-10ms at p50 in every row.

## Findings

- **Ingest is not the bottleneck.** Submit latency (`POST /v1/notes`,
  spool-to-disk + `202`) stays under 60 ms at p99 even at concurrency 50. The
  end-to-end latency is almost entirely queue wait + worker pipeline time.
- **Worker count scales throughput roughly linearly**, as expected for an
  I/O-bound pipeline: 8 workers moved sustained throughput from ~11 req/s to
  ~41 req/s at concurrency 50, and end-to-end p50 dropped from 4.4s to 1.1s.
  Rule of thumb from these numbers: throughput ≈ `QUEUE_WORKERS / per-job
  latency` once the client can sustain enough concurrent submissions to keep
  workers busy.
- **`QUEUE_MAX_DEPTH=100` backpressure works as designed.** At a 150-concurrent
  burst of 400 requests, both configurations reject the excess with clean
  `429` + `Retry-After` (282 and 273 rejections respectively) and accept the
  rest — no dropped or corrupted requests, no server errors, in either run.
- **Zero pipeline failures across ~2,300 total jobs** in this matrix — the
  queue/worker path held up under sustained and bursty load with the LLM/sink
  legs stubbed out.

## A load-test-tooling pitfall worth recording

The first attempt at the `concurrency=50, n=500` run produced ~130-200
spurious client-side errors (`dial tcp: can't assign requested address`).
Root cause: Go's default `http.Transport` keeps only 2 idle connections per
host, so many short-lived concurrent requests churn through ephemeral ports
faster than the OS reclaims them — a load-generator artifact, not a server
issue (the server's queue directory showed zero failures and zero rejections
for that run). Fixed by giving `tools/loadtest/gen`'s client a `Transport`
with `MaxIdleConnsPerHost` sized to the concurrency, which reuses connections
instead of opening a new one per request. All numbers above are from the
fixed generator.

## Separate incident during setup (not a load-test finding)

The first run of this exercise accidentally targeted a real, already-running
dev server on `:8080` (same port, real `LLM_API_KEY` and `WEBHOOK_URL`)
instead of the intended mock-backed instance, because the new server failed
to bind (`address already in use`) and the mistake wasn't caught before
sending traffic. 50 garbage-audio requests reached it; all failed OpenAI's
format-validation step immediately (`400 Invalid file format`), so no
billable transcription, no chat-completion calls, and no real webhook
delivery occurred. The resulting entries were removed from `queue/failed/`
with the user's confirmation. All runs after that point used dedicated
ports (`:18080`, `:18081`, `:19090`) with an upfront `lsof` check, and their
own scratch `QUEUE_DIR`.
