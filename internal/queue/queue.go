// Package queue turns the voice memo flow into durable background work: the
// upload is spooled to disk, then picked up by a worker, so a request is
// acknowledged without waiting for the LLM and survives a restart.
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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

// Processor is the domain entry point a worker drives.
type Processor interface {
	Process(ctx context.Context, filename string, audio io.Reader) (memo.Result, error)
}

// Queue is a directory of spooled jobs.
type Queue struct {
	dir  string
	proc Processor
	log  *slog.Logger
}

// New prepares dir as a queue root.
func New(dir string, proc Processor, log *slog.Logger) (*Queue, error) {
	for _, sub := range []string{dirTmp, dirPending, dirActive, dirDone} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("prepare queue dir: %w", err)
		}
	}
	return &Queue{dir: dir, proc: proc, log: log}, nil
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
