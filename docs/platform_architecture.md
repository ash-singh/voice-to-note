# VoiceLine — Platform Architecture (from scratch)

Scope: MVP, 100s–1000s of users, single region. Batch flow (record, then
upload — no live streaming transcription). Cross-platform mobile app.
AI layer mixes managed APIs and one self-hosted component. No
over-engineering for scale this app does not have yet.

## 1. Mobile app

**Stack: React Native (Expo).**

One codebase for iOS + Android. Small team, MVP scope — native Swift/Kotlin
  doubles build cost for no MVP benefit.


Skipped: live waveform streaming, on-device transcription, custom native
modules. Add only if offline-first transcription becomes a real requirement.

## 2. Backend

**Stack: Go**, same pattern as this repo — small binary, good concurrency
primitives for a worker pool, cheap to run, deploys as a single container
image to Cloud Run.

Building blocks (mirrors `internal/memo`, `internal/httpapi`,
`internal/queue`, `internal/sink` in this codebase — same separation
applies at any scale):

- **API layer** — `POST /v1/notes` (multipart upload) → `202` + job id.
  `GET /v1/notes/{id}` → job state + note once ready. Validates upload,
  hands off to the queue. No business logic here.
- **Queue** — **Cloud Tasks**, not the repo's directory-based queue. This
  repo's own `CLAUDE.md` flags that queue as single-instance-only
  (`Recover` sweeps `active/` on start, which would steal jobs from a
  sibling instance) — a real requirement once Cloud Run scales the worker
  past one instance. Cloud Tasks gives durable delivery, retry/backoff, and
  dead-lettering (via a failed-queue or Pub/Sub dead-letter topic) with no
  ops.
- **Worker(s)** — pull job → transcribe → extract structured note (LLM) →
  deliver to sink → update job state. Same `Transcriber` / `Analyzer` /
  `Sink` interface split this repo already uses — it is the right
  abstraction at this scale too, because there are genuinely 2–3
  interchangeable implementations behind each one (managed vs self-hosted
  ASR, different LLM providers, webhook vs Notion vs future sinks). Runs as
  a second Cloud Run service (or the same image, different entrypoint
  flag), invoked by Cloud Tasks over HTTP.
- **Metadata store** — **Cloud SQL for Postgres**. Job state, note
  content, user → note mapping. Swapping the file-based queue+lookup for a
  real DB also fixes the "two-sweep, can miss a job mid-move" caveat noted
  in this repo's `Lookup`.
- **Audio storage** — **GCS**, not local disk. Mobile clients upload
  directly via a signed URL where possible, to keep the API server thin
  and avoid proxying large files through it.
- **Secrets** — **Secret Manager** for `LLM_API_KEY`, sink credentials,
  etc. — mounted as env vars into Cloud Run, never baked into the image.

## 3. AI parts (mixed managed + self-hosted)

- **Transcription (ASR): managed to start** (OpenAI/Groq/Deepgram Whisper
  API). Self-host `faster-whisper` behind a small internal HTTP service
  **only once volume makes GPU cost cheaper than per-minute API pricing** —
  that crossover does not exist yet at 100s–1000s of users. Build the
  `Transcriber` interface so this swap is a config change, not a rewrite
  (this repo already does this).
- **Structured extraction (LLM): managed** (Claude/GPT, JSON mode). This is
  the quality-sensitive step — model quality and prompt iteration speed
  matter more than the per-call cost, and self-hosting an LLM at MVP volume
  is pure ops burden for no benefit.
- Both are called over plain HTTPS from the worker. `BaseURL` stays
  injectable (as in this repo) so self-hosted ASR is a URL change.

Skipped: fine-tuning, a model-serving platform (Triton/vLLM), GPU
autoscaling. Add self-hosted ASR serving only when the cost crossover
above is real.

## 4. Infra (GCP)

- **Compute:** one container image (API + worker share the binary,
  different entrypoint flag), run on **Cloud Run**, two services (API,
  worker) or one service with two revisions. No GKE at this scale — it is
  pure ops overhead for 1-2 services, and Cloud Run's scale-to-zero fits
  MVP traffic and cost better.
- **Queue:** **Cloud Tasks** (see §2).
- **DB:** **Cloud SQL for Postgres**, single instance (db-f1-micro/small
  tier is enough at this scale) + automated daily backups. Cloud Run
  connects via the Cloud SQL Auth Proxy sidecar/connector — no public IP
  on the DB.
- **Object storage:** **GCS** bucket with a lifecycle rule to delete raw
  audio after N days (cost control, and reduces what a breach could
  expose).
- **Secrets:** **Secret Manager**, referenced by Cloud Run, not env files.
- **IaC:** Terraform, one small module set — VPC connector, Cloud SQL
  instance, GCS bucket, Cloud Tasks queue, two Cloud Run services.
- **CI/CD:** GitHub Actions — build, test, `gofmt`/`vet`, build image,
  push to **Artifact Registry**, `gcloud run deploy` on merge to main.
  Same test/lint shape as this repo's existing `ci.yml`, one new deploy
  step.
- **Observability:** structured JSON logs (as this repo already does) —
  Cloud Run writes stdout straight to **Cloud Logging** with zero extra
  wiring. Cloud Monitoring for uptime/error-rate alerting. No dedicated
  tracing stack yet — one or two services, not worth it until there are
  several.

Single region, no multi-region failover, no read replicas, no service
mesh. Out of scope per goal — add when traffic or an SLA actually demands
it.

## 5. Communication between blocks

```
Mobile app                Backend API        Queue    Worker      ASR / LLM (AI)    Sink       Postgres    Push
   |                           |                |         |             |            |            |         |
   |-- POST /v1/notes -------->|                |         |             |            |            |         |
   |   (multipart audio)       |-- create job -------------------------------------------------->|         |
   |                           |-- enqueue ---->|         |             |            |            |         |
   |<-- 202 + job id ----------|                |         |             |            |            |         |
   |                           |                |-- job ->|             |            |            |         |
   |                           |                |         |-- audio --->|            |            |         |
   |                           |                |         |<- transcript|            |            |         |
   |                           |                |         |-- text ---->| (LLM)      |            |         |
   |                           |                |         |<- note -----|            |            |         |
   |                           |                |         |-- deliver note --------->|            |         |
   |                           |                |         |-- update job -------------------------------->|
   |                           |                |         |-- notify -------------------------------------------->|
   |                           |                |         |             |            |            |         |
   |-- GET /v1/notes/{id} ---->|                |         |             |            |            |         |
   |   (poll, fallback)        |-- read job ----------------------------------------------------->|         |
   |<-- state + note ----------|                |         |             |            |            |         |
```

- **App ↔ API:** HTTPS/REST + JSON, multipart for upload. Async by design
  — the client never blocks on transcription, matching the "202 now, state
  later" contract this repo already uses.
- **API ↔ Queue ↔ Worker:** enqueue/dequeue only, no direct API→Worker
  call. This is what makes the API layer stay thin and lets workers scale
  independently.
- **Worker ↔ AI services:** plain HTTPS calls to external APIs (managed)
  or an internal service URL (self-hosted). Same interface either way.
  Retries only on errors that provably didn't reach the sink (as this
  repo's `SinkError` split already enforces) — never blind-retry a
  delivery that might have gone through.
- **Worker → DB:** job state transitions, written once per stage — this is
  what the API's poll/push both read from.
- **Backend → App:** push notification is the primary "it's ready" signal;
  polling `GET /v1/notes/{id}` is the fallback for when push is missed or
  disabled.

## Open questions worth deciding before writing code

- Presigned direct-to-S3 upload from the app, or upload through the API?
  (Direct is leaner; through-API is simpler auth/validation.)
- Which sinks does v1 actually need? (Affects nothing else — this is
  additive behind the existing `Sink` interface.)
- Retention window for raw audio once a note is extracted.
