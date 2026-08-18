// Package queue turns the voice memo flow into durable background work: the
// upload is spooled to disk, then picked up by a worker, so a request is
// acknowledged without waiting for the LLM and survives a restart.
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ash-singh/voice-to-note/internal/logging"
	"github.com/ash-singh/voice-to-note/internal/memo"
)

// Queue subdirectories. A job is only ever visible to a worker once it has been
// renamed into pending, which is what makes a partial upload unobservable.
const (
	dirTmp     = "tmp"
	dirPending = "pending"
	dirActive  = "active"
	dirDone    = "done"
	dirFailed  = "failed"
)

// maxAttempts bounds retries of a job. Fixed rather than configurable: it is not
// a value this service has any reason to tune per deployment yet.
const maxAttempts = 3

// retryDelay backs off exponentially from 30s, which is the right order of
// magnitude for the rate limits that cause most retries.
func retryDelay(attempt int) time.Duration {
	return 30 * time.Second << (attempt - 1)
}

// idBytes is how much of the content hash names a job. 8 bytes is ample to keep
// distinct recordings apart, and a short id stays readable in logs and URLs.
const idBytes = 8

// A job file is named "<due>-<attempt>-<id><ext>": the due time comes first and
// is zero padded, so the lexical order os.ReadDir returns is also the order the
// jobs become due, and the oldest due job is simply the first entry.
const dueDigits = 10

func jobName(due time.Time, attempt int, id, ext string) string {
	return fmt.Sprintf("%0*d-%d-%s%s", dueDigits, due.Unix(), attempt, id, ext)
}

// parseJobName splits a job filename back into its parts.
func parseJobName(name string) (due time.Time, attempt int, id string, err error) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	parts := strings.SplitN(base, "-", 3)
	if len(parts) != 3 {
		return time.Time{}, 0, "", fmt.Errorf("malformed job name %q", name)
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, 0, "", fmt.Errorf("malformed job name %q: %w", name, err)
	}
	attempt, err = strconv.Atoi(parts[1])
	if err != nil {
		return time.Time{}, 0, "", fmt.Errorf("malformed job name %q: %w", name, err)
	}
	return time.Unix(sec, 0), attempt, parts[2], nil
}

// State is where a job has got to.
type State string

// Job states. Processing and queued are distinguished because a stuck job shows
// up as processing, which is what you want to see when diagnosing one.
const (
	StateQueued     State = "queued"
	StateProcessing State = "processing"
	StateDone       State = "done"
	StateFailed     State = "failed"
)

// ErrJobNotFound means no job with that id was ever enqueued, or its record has
// since been swept.
var ErrJobNotFound = errors.New("job not found")

// Status is what a caller can learn about a job after the request that created
// it has returned. Result is set once the job is done.
type Status struct {
	ID     string       `json:"id"`
	State  State        `json:"state"`
	Result *memo.Result `json:"result,omitempty"`
	Reason string       `json:"reason,omitempty"`
}

// Processor is the domain entry point a worker drives.
type Processor interface {
	Process(ctx context.Context, filename string, audio io.Reader) (memo.Result, error)
}

// Queue is a directory of spooled jobs.
type Queue struct {
	dir     string
	proc    Processor
	timeout time.Duration
	log     *slog.Logger
}

// New prepares dir as a queue root. timeout bounds one job: the request that
// enqueued it is long gone, so its context cannot carry the deadline.
func New(dir string, proc Processor, timeout time.Duration, log *slog.Logger) (*Queue, error) {
	for _, sub := range []string{dirTmp, dirPending, dirActive, dirDone, dirFailed} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("prepare queue dir: %w", err)
		}
	}
	return &Queue{dir: dir, proc: proc, timeout: timeout, log: log}, nil
}

// Enqueue spools audio and returns the job id. The id is a hash of the audio, so
// the same recording submitted twice is the same job.
func (q *Queue) Enqueue(filename string, audio io.Reader) (string, error) {
	spool, err := os.CreateTemp(q.path(dirTmp), "upload-*")
	if err != nil {
		return "", fmt.Errorf("spool upload: %w", err)
	}
	defer os.Remove(spool.Name()) // no-op once the rename below has moved it

	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(spool, sum), audio); err != nil {
		spool.Close()
		return "", fmt.Errorf("spool upload: %w", err)
	}
	if err := spool.Close(); err != nil {
		return "", fmt.Errorf("spool upload: %w", err)
	}

	id := hex.EncodeToString(sum.Sum(nil)[:idBytes])
	known, err := q.known(id)
	if err != nil {
		return "", err
	}
	if known {
		return id, nil
	}

	name := jobName(time.Now(), 0, id, filepath.Ext(filename))
	if err := os.Rename(spool.Name(), q.path(dirPending, name)); err != nil {
		return "", fmt.Errorf("enqueue job: %w", err)
	}
	return id, nil
}

// known reports whether id is already queued, in flight or finished. The id is a
// content hash, so this is what makes a re-uploaded recording a no-op.
func (q *Queue) known(id string) (bool, error) {
	_, err := q.Lookup(id)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrJobNotFound):
		return false, nil
	default:
		return false, err
	}
}

// idlePoll is how long a worker waits before looking for work again. A voice
// memo is not latency sensitive, so polling beats a filesystem watcher.
const idlePoll = time.Second

// Run drives workers until ctx is cancelled, then returns once they have all
// finished the job in hand.
func (q *Queue) Run(ctx context.Context, workers int) {
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.work(ctx)
		}()
	}
	wg.Wait()
}

// work claims jobs back to back while there are any, and idles otherwise.
func (q *Queue) work(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		claimed, err := q.ProcessNext(ctx)
		if err != nil {
			q.log.ErrorContext(ctx, "job failed", "error", err)
		}
		if claimed {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(idlePoll):
		}
	}
}

// Recover clears out jobs left in flight by a crash. It must run before any
// worker starts, and assumes this process is the only one using the directory.
func (q *Queue) Recover(ctx context.Context) error {
	entries, err := os.ReadDir(q.path(dirActive))
	if err != nil {
		return fmt.Errorf("read active: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		id := idOf(name)
		// A recorded result means the work finished and only the cleanup was
		// lost, so the leftover file is all there is to discard.
		if _, err := os.Stat(q.path(dirDone, id+".json")); err == nil {
			q.log.InfoContext(ctx, "discarding a job that finished before the restart", "job_id", id)
			if err := os.Remove(q.path(dirActive, name)); err != nil {
				return fmt.Errorf("discard finished job %s: %w", id, err)
			}
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("recover job %s: %w", id, err)
		}

		// Otherwise the job died somewhere between transcription and recording
		// its result, so whether the note reached the sink is unknown. Retrying
		// could duplicate it; a human decides.
		q.log.WarnContext(ctx, "job interrupted by a restart, needs review", "job_id", id)
		if err := q.deadLetter(name, id, "interrupted by a restart; check the sink before retrying"); err != nil {
			return err
		}
	}
	return nil
}

// ProcessNext claims and runs one job. It reports whether a job was claimed.
func (q *Queue) ProcessNext(ctx context.Context) (bool, error) {
	entries, err := os.ReadDir(q.path(dirPending))
	if err != nil {
		return false, fmt.Errorf("read pending: %w", err)
	}

	now := time.Now()
	for _, entry := range entries {
		name := entry.Name()
		due, attempt, _, err := parseJobName(name)
		if err != nil {
			q.log.ErrorContext(ctx, "ignoring malformed job file", "name", name, "error", err)
			continue
		}
		if due.After(now) {
			continue // still backing off from an earlier attempt
		}
		// Winning this rename is what claims the job: a racing worker gets
		// ENOENT and moves on, so no lock is needed.
		if err := os.Rename(q.path(dirPending, name), q.path(dirActive, name)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("claim job: %w", err)
		}
		if err := q.run(ctx, name); err != nil {
			return true, q.handleFailure(ctx, name, attempt, err)
		}
		return true, nil
	}
	return false, nil
}

// handleFailure decides what happens to a job whose attempt failed, and returns the
// original failure so the caller can log it.
func (q *Queue) handleFailure(ctx context.Context, name string, attempt int, cause error) error {
	_, _, id, _ := parseJobName(name)
	next := attempt + 1

	var sinkErr *memo.SinkError
	switch {
	case errors.As(cause, &sinkErr):
		// The sink may have stored the note before failing, so retrying risks a
		// duplicate. A human decides this one.
		return errors.Join(cause, q.deadLetter(name, id, "delivery to "+sinkErr.Sink+" failed and may have partly succeeded: "+cause.Error()))
	case errors.Is(cause, memo.ErrEmptyTranscript):
		return errors.Join(cause, q.deadLetter(name, id, cause.Error()))
	case next >= maxAttempts:
		return errors.Join(cause, q.deadLetter(name, id, fmt.Sprintf("gave up after %d attempts: %v", next, cause)))
	}

	retryAt := time.Now().Add(retryDelay(next))
	q.log.WarnContext(ctx, "job failed, retrying later",
		"attempt", next, "retry_at", retryAt.Format(time.RFC3339), "error", cause)
	if err := os.Rename(q.path(dirActive, name), q.path(dirPending, jobName(retryAt, next, id, filepath.Ext(name)))); err != nil {
		return errors.Join(cause, fmt.Errorf("reschedule job %s: %w", id, err))
	}
	return cause
}

// deadLetter parks a job for a human, recording why it stopped.
func (q *Queue) deadLetter(name, id, reason string) error {
	if err := os.Rename(q.path(dirActive, name), q.path(dirFailed, name)); err != nil {
		return fmt.Errorf("dead letter job %s: %w", id, err)
	}
	return os.WriteFile(q.path(dirFailed, id+".reason"), []byte(reason), 0o640)
}

// run processes a claimed job.
func (q *Queue) run(ctx context.Context, name string) error {
	file, err := os.Open(q.path(dirActive, name))
	if err != nil {
		return fmt.Errorf("open claimed job: %w", err)
	}
	defer file.Close()

	// The submitting request is gone, so the job id becomes the correlation id
	// every log record from this job carries.
	ctx = logging.WithRequestID(ctx, "job-"+idOf(name))
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()

	result, err := q.proc.Process(ctx, name, file)
	if err != nil {
		return err
	}
	if err := q.record(idOf(name), result); err != nil {
		return err
	}
	return os.Remove(q.path(dirActive, name))
}

// record stores the outcome so it can be read back after the request that
// enqueued the job is long gone. Written via tmp and renamed, so a reader never
// observes half a document.
func (q *Queue) record(id string, result memo.Result) error {
	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	tmp := q.path(dirTmp, "result-"+id+".json")
	if err := os.WriteFile(tmp, body, 0o640); err != nil {
		return fmt.Errorf("record result: %w", err)
	}
	if err := os.Rename(tmp, q.path(dirDone, id+".json")); err != nil {
		return fmt.Errorf("record result: %w", err)
	}
	return nil
}

// idOf recovers the job id from a job filename, ignoring a malformed name: the
// caller has already read the file, so there is nothing better to report.
func idOf(name string) string {
	_, _, id, err := parseJobName(name)
	if err != nil {
		return name
	}
	return id
}

func (q *Queue) path(elem ...string) string {
	return filepath.Join(append([]string{q.dir}, elem...)...)
}

// Lookup reports the state of a job, and its result once it has one.
func (q *Queue) Lookup(id string) (Status, error) {
	body, err := os.ReadFile(q.path(dirDone, id+".json"))
	switch {
	case err == nil:
		var result memo.Result
		if err := json.Unmarshal(body, &result); err != nil {
			return Status{}, fmt.Errorf("decode result of job %s: %w", id, err)
		}
		return Status{ID: id, State: StateDone, Result: &result}, nil
	case !os.IsNotExist(err):
		return Status{}, fmt.Errorf("look up job %s: %w", id, err)
	}

	if reason, err := os.ReadFile(q.path(dirFailed, id+".reason")); err == nil {
		return Status{ID: id, State: StateFailed, Reason: string(reason)}, nil
	} else if !os.IsNotExist(err) {
		return Status{}, fmt.Errorf("look up job %s: %w", id, err)
	}

	for state, sub := range map[State]string{StateQueued: dirPending, StateProcessing: dirActive} {
		matches, err := filepath.Glob(q.path(sub, "*-"+id+".*"))
		if err != nil {
			return Status{}, fmt.Errorf("look up job %s: %w", id, err)
		}
		if len(matches) > 0 {
			return Status{ID: id, State: state}, nil
		}
	}
	return Status{}, fmt.Errorf("%s: %w", id, ErrJobNotFound)
}
