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
	"strings"
	"sync"
	"time"

	"github.com/ash-singh/voice-to-note/internal/memo"
)

// Queue subdirectories. A job is only ever visible to a worker once it has been
// renamed into pending, which is what makes a partial upload unobservable.
const (
	dirTmp     = "tmp"
	dirPending = "pending"
	dirActive  = "active"
	dirDone    = "done"
)

// idBytes is how much of the content hash names a job. 8 bytes is ample to keep
// distinct recordings apart, and a short id stays readable in logs and URLs.
const idBytes = 8

// State is where a job has got to.
type State string

// Job states. Processing and queued are distinguished because a stuck job shows
// up as processing, which is what you want to see when diagnosing one.
const (
	StateQueued     State = "queued"
	StateProcessing State = "processing"
	StateDone       State = "done"
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
	for _, sub := range []string{dirTmp, dirPending, dirActive, dirDone} {
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

	name := id + filepath.Ext(filename)
	if err := os.Rename(spool.Name(), q.path(dirPending, name)); err != nil {
		return "", fmt.Errorf("enqueue job: %w", err)
	}
	return id, nil
}

// known reports whether id is already queued, in flight or finished. The id is a
// content hash, so this is what makes a re-uploaded recording a no-op.
func (q *Queue) known(id string) (bool, error) {
	for _, sub := range []string{dirPending, dirActive, dirDone} {
		matches, err := filepath.Glob(q.path(sub, id+".*"))
		if err != nil {
			return false, fmt.Errorf("look up job %s: %w", id, err)
		}
		if len(matches) > 0 {
			return true, nil
		}
	}
	return false, nil
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

// ProcessNext claims and runs one job. It reports whether a job was claimed.
func (q *Queue) ProcessNext(ctx context.Context) (bool, error) {
	entries, err := os.ReadDir(q.path(dirPending))
	if err != nil {
		return false, fmt.Errorf("read pending: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		// Winning this rename is what claims the job: a racing worker gets
		// ENOENT and moves on, so no lock is needed.
		if err := os.Rename(q.path(dirPending, name), q.path(dirActive, name)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("claim job: %w", err)
		}
		return true, q.run(ctx, name)
	}
	return false, nil
}

// run processes a claimed job.
func (q *Queue) run(ctx context.Context, name string) error {
	file, err := os.Open(q.path(dirActive, name))
	if err != nil {
		return fmt.Errorf("open claimed job: %w", err)
	}
	defer file.Close()

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

// idOf recovers the job id from a job filename, which is the id plus the audio
// extension the transcription API needs.
func idOf(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
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

	for state, sub := range map[State]string{StateQueued: dirPending, StateProcessing: dirActive} {
		matches, err := filepath.Glob(q.path(sub, id+".*"))
		if err != nil {
			return Status{}, fmt.Errorf("look up job %s: %w", id, err)
		}
		if len(matches) > 0 {
			return Status{ID: id, State: state}, nil
		}
	}
	return Status{}, fmt.Errorf("%s: %w", id, ErrJobNotFound)
}
