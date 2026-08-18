package queue_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ash-singh/voice-to-note/internal/memo"
	"github.com/ash-singh/voice-to-note/internal/queue"
)

// fakeProcessor records what the worker handed it, so tests can assert on the
// filename and the bytes without touching the network.
type fakeProcessor struct {
	mu        sync.Mutex
	filenames []string
	bodies    []string
	result    memo.Result
	err       error
}

func (f *fakeProcessor) Process(_ context.Context, filename string, audio io.Reader) (memo.Result, error) {
	body, err := io.ReadAll(audio)
	if err != nil {
		return memo.Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filenames = append(f.filenames, filename)
	f.bodies = append(f.bodies, string(body))
	return f.result, f.err
}

func (f *fakeProcessor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.filenames)
}

func newTestQueue(t *testing.T, proc queue.Processor) *queue.Queue {
	t.Helper()
	q, err := queue.New(t.TempDir(), proc, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	return q
}

// Claiming is the load-bearing property: the queue is safe for concurrent
// workers only because a job is owned by whoever wins the rename.
func TestProcessNextClaimsAJobExactlyOnce(t *testing.T) {
	proc := &fakeProcessor{}
	q := newTestQueue(t, proc)
	if _, err := q.Enqueue("memo.m4a", strings.NewReader("audio bytes")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	const workers = 4
	claimed := make([]bool, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed[i], errs[i] = q.ProcessNext(context.Background())
		}()
	}
	wg.Wait()

	wins := 0
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: ProcessNext: %v", i, errs[i])
		}
		if claimed[i] {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("workers reporting a claimed job = %d, want 1", wins)
	}
	if got := proc.callCount(); got != 1 {
		t.Errorf("Process calls = %d, want 1", got)
	}
}

// openai.go infers the audio format from the multipart filename, so the queue
// must not drop the extension when it renames the upload to a job id.
func TestProcessNextPassesTheAudioAndItsExtension(t *testing.T) {
	proc := &fakeProcessor{}
	q := newTestQueue(t, proc)
	if _, err := q.Enqueue("Voice Memo 3.m4a", strings.NewReader("audio bytes")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if _, err := q.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if got := len(proc.filenames); got != 1 {
		t.Fatalf("Process calls = %d, want 1", got)
	}
	if got := filepath.Ext(proc.filenames[0]); got != ".m4a" {
		t.Errorf("filename %q has extension %q, want %q", proc.filenames[0], got, ".m4a")
	}
	if got := proc.bodies[0]; got != "audio bytes" {
		t.Errorf("audio = %q, want %q", got, "audio bytes")
	}
}

// Async processing removes the response body that carried the note, so the
// outcome has to be recorded somewhere the caller can read it back later.
func TestProcessNextRecordsTheResult(t *testing.T) {
	want := memo.Result{
		Note:    memo.Note{Title: "Invoice", ActionItems: []string{"Call Anna"}},
		Sink:    "notion",
		SinkRef: "https://notion.so/page",
	}
	proc := &fakeProcessor{result: want}
	dir := t.TempDir()
	q, err := queue.New(dir, proc, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	id, err := q.Enqueue("memo.m4a", strings.NewReader("audio bytes"))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if _, err := q.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "done", id+".json"))
	if err != nil {
		t.Fatalf("read recorded result: %v", err)
	}
	var got memo.Result
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode recorded result: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recorded result = %+v, want %+v", got, want)
	}
}

// A client retrying an upload it already got a 202 for must not cause the memo
// to be transcribed and delivered a second time.
func TestEnqueueIsIdempotentForACompletedJob(t *testing.T) {
	proc := &fakeProcessor{}
	q := newTestQueue(t, proc)
	audio := "audio bytes"
	first, err := q.Enqueue("memo.m4a", strings.NewReader(audio))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	second, err := q.Enqueue("memo.m4a", strings.NewReader(audio))
	if err != nil {
		t.Fatalf("re-Enqueue: %v", err)
	}

	if second != first {
		t.Errorf("second Enqueue id = %q, want the first id %q", second, first)
	}
	claimed, err := q.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext after re-Enqueue: %v", err)
	}
	if claimed {
		t.Error("re-enqueueing a completed job produced new work, want none")
	}
	if got := proc.callCount(); got != 1 {
		t.Errorf("Process calls = %d, want 1", got)
	}
}

// blockingReader delivers its first half, then waits until released, so a test
// can inspect the queue while an upload is still streaming in.
type blockingReader struct {
	rest     string
	started  chan struct{}
	release  chan struct{}
	sentHead bool
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if !b.sentHead {
		b.sentHead = true
		close(b.started)
		return copy(p, "first half "), nil
	}
	<-b.release
	if b.rest == "" {
		return 0, io.EOF
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	return n, nil
}

// An upload in progress must be invisible to workers: a job appears in pending
// only once all of its bytes are on disk, otherwise a worker transcribes a
// truncated recording.
func TestEnqueueHidesAPartialUpload(t *testing.T) {
	proc := &fakeProcessor{}
	q := newTestQueue(t, proc)
	audio := &blockingReader{rest: "second half", started: make(chan struct{}), release: make(chan struct{})}

	done := make(chan error, 1)
	go func() {
		_, err := q.Enqueue("memo.m4a", audio)
		done <- err
	}()
	<-audio.started

	claimed, err := q.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext mid-upload: %v", err)
	}
	if claimed {
		t.Error("a worker claimed a job while its upload was still streaming")
	}

	close(audio.release)
	if err := <-done; err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext after upload: %v", err)
	}
	if got := proc.bodies[0]; got != "first half second half" {
		t.Errorf("audio = %q, want the whole upload", got)
	}
}

// gatedProcessor blocks inside Process until released, so a test can enqueue
// while a job is genuinely in flight.
type gatedProcessor struct {
	fakeProcessor
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedProcessor) Process(ctx context.Context, filename string, audio io.Reader) (memo.Result, error) {
	// Once, so an unexpected second call fails an assertion rather than
	// panicking on a closed channel.
	g.once.Do(func() { close(g.started) })
	<-g.release
	return g.fakeProcessor.Process(ctx, filename, audio)
}

// The duplicate that costs real money: re-uploading while the first copy is
// mid-flight must not queue a second delivery to the sink.
func TestEnqueueIsIdempotentWhileAJobIsInFlight(t *testing.T) {
	proc := &gatedProcessor{started: make(chan struct{}), release: make(chan struct{})}
	q := newTestQueue(t, proc)
	audio := "audio bytes"
	if _, err := q.Enqueue("memo.m4a", strings.NewReader(audio)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	running := make(chan error, 1)
	go func() {
		_, err := q.ProcessNext(context.Background())
		running <- err
	}()
	<-proc.started

	if _, err := q.Enqueue("memo.m4a", strings.NewReader(audio)); err != nil {
		t.Fatalf("re-Enqueue: %v", err)
	}

	close(proc.release)
	if err := <-running; err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	claimed, err := q.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext after re-Enqueue: %v", err)
	}
	if claimed {
		t.Error("re-enqueueing an in-flight job produced new work, want none")
	}
	if got := proc.callCount(); got != 1 {
		t.Errorf("Process calls = %d, want 1", got)
	}
}
