package queue_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ash-singh/voice-to-note/internal/logging"
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
	q, err := queue.New(t.TempDir(), proc, time.Minute, slog.New(slog.DiscardHandler))
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
	q, err := queue.New(dir, proc, time.Minute, slog.New(slog.DiscardHandler))
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

// signalProcessor reports each processed job, so tests can wait on progress
// instead of sleeping.
type signalProcessor struct {
	fakeProcessor
	calls chan string
}

func (s *signalProcessor) Process(ctx context.Context, filename string, audio io.Reader) (memo.Result, error) {
	result, err := s.fakeProcessor.Process(ctx, filename, audio)
	s.calls <- filename
	return result, err
}

// Run is what makes the queue drain on its own: workers keep claiming jobs
// until the server shuts down, at which point Run must return.
func TestRunDrainsTheQueueAndStopsOnContextCancel(t *testing.T) {
	proc := &signalProcessor{calls: make(chan string, 3)}
	q := newTestQueue(t, proc)
	for _, audio := range []string{"one", "two", "three"} {
		if _, err := q.Enqueue("memo.m4a", strings.NewReader(audio)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		q.Run(ctx, 2)
		close(stopped)
	}()

	for i := range 3 {
		select {
		case <-proc.calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 jobs processed before timing out", i)
		}
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// cancellingProcessor cancels the run context from inside the first job, the way
// a SIGTERM arrives mid-flight.
type cancellingProcessor struct {
	fakeProcessor
	cancel func()
}

func (c *cancellingProcessor) Process(ctx context.Context, filename string, audio io.Reader) (memo.Result, error) {
	result, err := c.fakeProcessor.Process(ctx, filename, audio)
	c.cancel()
	return result, err
}

// On shutdown a worker must finish the job in hand and then stop, rather than
// draining the whole backlog past its deadline.
func TestRunStopsClaimingNewWorkOnceCancelled(t *testing.T) {
	proc := &cancellingProcessor{}
	q := newTestQueue(t, proc)
	for _, audio := range []string{"one", "two"} {
		if _, err := q.Enqueue("memo.m4a", strings.NewReader(audio)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc.cancel = cancel

	stopped := make(chan struct{})
	go func() {
		q.Run(ctx, 1)
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if got := proc.callCount(); got != 1 {
		t.Errorf("jobs processed after cancel = %d, want 1", got)
	}
}

// Async processing only works if the caller can find out what happened, so
// Lookup is the replacement for the note the 201 body used to carry.
func TestLookupReportsJobState(t *testing.T) {
	t.Run("queued", func(t *testing.T) {
		q := newTestQueue(t, &fakeProcessor{})
		id, err := q.Enqueue("memo.m4a", strings.NewReader("audio bytes"))
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		got, err := q.Lookup(id)
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if got.ID != id || got.State != queue.StateQueued {
			t.Errorf("status = %+v, want id %s in state %s", got, id, queue.StateQueued)
		}
		if got.Result != nil {
			t.Errorf("queued job carries a result: %+v", got.Result)
		}
	})

	t.Run("done carries the result", func(t *testing.T) {
		want := memo.Result{Note: memo.Note{Title: "Invoice"}, Sink: "notion", SinkRef: "https://notion.so/page"}
		q := newTestQueue(t, &fakeProcessor{result: want})
		id, err := q.Enqueue("memo.m4a", strings.NewReader("audio bytes"))
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if _, err := q.ProcessNext(context.Background()); err != nil {
			t.Fatalf("ProcessNext: %v", err)
		}

		got, err := q.Lookup(id)
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if got.State != queue.StateDone {
			t.Errorf("state = %s, want %s", got.State, queue.StateDone)
		}
		if got.Result == nil || !reflect.DeepEqual(*got.Result, want) {
			t.Errorf("result = %+v, want %+v", got.Result, want)
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		q := newTestQueue(t, &fakeProcessor{})

		_, err := q.Lookup("0123456789abcdef")

		if !errors.Is(err, queue.ErrJobNotFound) {
			t.Errorf("error = %v, want ErrJobNotFound", err)
		}
	})
}

// deadlineProcessor reports the deadline it was given.
type deadlineProcessor struct {
	fakeProcessor
	hasDeadline bool
	within      time.Duration
}

func (d *deadlineProcessor) Process(ctx context.Context, filename string, audio io.Reader) (memo.Result, error) {
	deadline, ok := ctx.Deadline()
	d.hasDeadline = ok
	if ok {
		d.within = time.Until(deadline)
	}
	return d.fakeProcessor.Process(ctx, filename, audio)
}

// PROCESS_TIMEOUT bounds the whole pipeline. Once the work moves off the request
// there is no request context to carry it, so the queue has to apply it.
func TestProcessNextBoundsAJobByTheProcessTimeout(t *testing.T) {
	proc := &deadlineProcessor{}
	q, err := queue.New(t.TempDir(), proc, 90*time.Second, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	if _, err := q.Enqueue("memo.m4a", strings.NewReader("audio bytes")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if _, err := q.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if !proc.hasDeadline {
		t.Fatal("the job context has no deadline, so a stuck pipeline holds a worker forever")
	}
	if proc.within <= 0 || proc.within > 90*time.Second {
		t.Errorf("deadline is %v away, want it within the 90s timeout", proc.within)
	}
}

// taggingProcessor captures the correlation id the queue put on the job context.
type taggingProcessor struct {
	fakeProcessor
	requestID string
}

func (p *taggingProcessor) Process(ctx context.Context, filename string, audio io.Reader) (memo.Result, error) {
	p.requestID = logging.RequestIDFrom(ctx)
	return p.fakeProcessor.Process(ctx, filename, audio)
}

// The request that enqueued the job is gone, so its request id cannot correlate
// the worker's log lines. The job id has to take over, or "voice memo stored"
// lands in the log with nothing tying it to anything.
func TestProcessNextCorrelatesLogsWithTheJobID(t *testing.T) {
	proc := &taggingProcessor{}
	q := newTestQueue(t, proc)
	id, err := q.Enqueue("memo.m4a", strings.NewReader("audio bytes"))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if _, err := q.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if !strings.Contains(proc.requestID, id) {
		t.Errorf("job context request id = %q, want it to contain the job id %q", proc.requestID, id)
	}
}
