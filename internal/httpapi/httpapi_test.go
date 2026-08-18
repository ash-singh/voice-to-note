package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ash-singh/voice-to-note/internal/httpapi"
	"github.com/ash-singh/voice-to-note/internal/logging"
	"github.com/ash-singh/voice-to-note/internal/memo"
	"github.com/ash-singh/voice-to-note/internal/queue"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

func audioBody(t *testing.T, field, filename, content string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := io.WriteString(part, content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func newServer(t *testing.T, q httpapi.Jobs, maxBytes int64) (*gin.Engine, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log := logging.New("debug", &logs)
	return httpapi.NewRouter(httpapi.NewNoteHandler(q, maxBytes, log), log), &logs
}

func TestCreateNoteRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name       string
		field      string
		filename   string
		content    string
		maxBytes   int64
		enqueueErr error
		wantStatus int
	}{
		{name: "missing audio field", field: "file", filename: "memo.m4a", content: "x", maxBytes: 1 << 20, wantStatus: http.StatusBadRequest},
		{name: "unsupported format", field: "audio", filename: "memo.txt", content: "x", maxBytes: 1 << 20, wantStatus: http.StatusUnsupportedMediaType},
		{name: "audio too large", field: "audio", filename: "memo.m4a", content: strings.Repeat("x", 2048), maxBytes: 512, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "queue unavailable", field: "audio", filename: "memo.m4a", content: "x", maxBytes: 1 << 20, enqueueErr: errors.New("disk full"), wantStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, _ := newServer(t, &fakeQueue{enqueueErr: tt.enqueueErr}, tt.maxBytes)
			body, contentType := audioBody(t, tt.field, tt.filename, tt.content)
			req := httptest.NewRequest(http.MethodPost, "/v1/notes", body)
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body)
			}
			var got struct{ Error string }
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error == "" {
				t.Errorf("want error envelope, got %s", rec.Body)
			}
		})
	}
}

func TestRequestIDIsReusedFromInboundHeader(t *testing.T) {
	router, logs := newServer(t, &fakeQueue{}, 1<<20)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "req-from-client")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "req-from-client" {
		t.Errorf("X-Request-Id = %q, want req-from-client", got)
	}
	if !strings.Contains(logs.String(), "req-from-client") {
		t.Errorf("access log missing request id: %s", logs.String())
	}
}

func TestRecoveryLogsPanicAndReturns500(t *testing.T) {
	var logs bytes.Buffer
	log := logging.New("debug", &logs)
	r := gin.New()
	r.Use(httpapi.RequestID(), httpapi.RequestLogger(log), httpapi.Recovery(log))
	r.GET("/boom", func(*gin.Context) { panic("kaboom") })
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "panic recovered") {
		t.Errorf("panic not logged: %s", logs.String())
	}
}

// fakeQueue stands in for internal/queue in handler tests.
type fakeQueue struct {
	id         string
	enqueueErr error
	gotName    string
	gotAudio   string
	status     queue.Status
	lookupErr  error
	gotLookup  string
}

func (f *fakeQueue) Enqueue(filename string, audio io.Reader) (string, error) {
	b, _ := io.ReadAll(audio)
	f.gotName, f.gotAudio = filename, string(b)
	return f.id, f.enqueueErr
}

// The point of the queue: the request is acknowledged without waiting for the
// LLM, and the caller gets an id to follow up with.
func TestCreateNoteAcceptsTheUploadAndReturnsAJobID(t *testing.T) {
	q := &fakeQueue{id: "abc123"}
	router, _ := newServer(t, q, 1<<20)
	body, contentType := audioBody(t, "audio", "memo.m4a", "fake-audio")
	req := httptest.NewRequest(http.MethodPost, "/v1/notes", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body)
	}
	var got struct {
		Data struct {
			JobID string `json:"job_id"`
			State string `json:"state"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.Data.JobID != "abc123" || got.Data.State != "queued" {
		t.Errorf("response data = %+v", got.Data)
	}
	if want := "/v1/notes/abc123"; rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	if q.gotName != "memo.m4a" || q.gotAudio != "fake-audio" {
		t.Errorf("queue got (%q, %q)", q.gotName, q.gotAudio)
	}
}

func (f *fakeQueue) Lookup(id string) (queue.Status, error) {
	f.gotLookup = id
	return f.status, f.lookupErr
}

// The job id from the 202 has to lead somewhere, or async processing just hides
// failures from the caller.
func TestGetNoteReturnsTheJobStatus(t *testing.T) {
	result := memo.Result{Note: memo.Note{Title: "Invoice"}, Sink: "notion", SinkRef: "https://notion.so/page"}
	q := &fakeQueue{status: queue.Status{ID: "abc123", State: queue.StateDone, Result: &result}}
	router, _ := newServer(t, q, 1<<20)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/notes/abc123", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var got struct{ Data queue.Status }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.Data.State != queue.StateDone || got.Data.Result == nil || got.Data.Result.Note.Title != "Invoice" {
		t.Errorf("response data = %+v", got.Data)
	}
	if q.gotLookup != "abc123" {
		t.Errorf("looked up %q, want abc123", q.gotLookup)
	}
}

func TestGetNoteReturns404ForAnUnknownJob(t *testing.T) {
	q := &fakeQueue{lookupErr: queue.ErrJobNotFound}
	router, _ := newServer(t, q, 1<<20)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/notes/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	var got struct{ Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error == "" {
		t.Errorf("want error envelope, got %s", rec.Body)
	}
}

// End to end over the real queue: the work outlives the request that submitted
// it, which is the whole point of the 202.
func TestQueuedJobCompletesAfterTheRequestReturns(t *testing.T) {
	proc := &recordingProcessor{result: memo.Result{Note: memo.Note{Title: "Invoice"}, Sink: "webhook", SinkRef: "row-1"}}
	var logs bytes.Buffer
	log := logging.New("debug", &logs)
	q, err := queue.New(t.TempDir(), proc, time.Minute, log)
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	router := httpapi.NewRouter(httpapi.NewNoteHandler(q, 1<<20, log), log)

	body, contentType := audioBody(t, "audio", "memo.m4a", "fake-audio")
	req := httptest.NewRequest(http.MethodPost, "/v1/notes", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202 (body %s)", rec.Code, rec.Body)
	}
	var accepted struct {
		Data struct {
			JobID string `json:"job_id"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}

	// The request is over; a worker picks the job up afterwards.
	claimed, err := q.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !claimed {
		t.Fatal("no job was queued by the request")
	}

	statusRec := httptest.NewRecorder()
	router.ServeHTTP(statusRec, httptest.NewRequest(http.MethodGet, "/v1/notes/"+accepted.Data.JobID, nil))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (body %s)", statusRec.Code, statusRec.Body)
	}
	var got struct{ Data queue.Status }
	if err := json.Unmarshal(statusRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("status response is not JSON: %v", err)
	}
	if got.Data.State != queue.StateDone || got.Data.Result == nil || got.Data.Result.SinkRef != "row-1" {
		t.Errorf("status = %+v", got.Data)
	}
	if proc.gotAudio != "fake-audio" {
		t.Errorf("processor got audio %q, want the uploaded bytes", proc.gotAudio)
	}
}

// recordingProcessor is the real memo.Service's stand-in for the wiring test.
type recordingProcessor struct {
	result   memo.Result
	gotName  string
	gotAudio string
}

func (r *recordingProcessor) Process(_ context.Context, filename string, audio io.Reader) (memo.Result, error) {
	b, err := io.ReadAll(audio)
	if err != nil {
		return memo.Result{}, err
	}
	r.gotName, r.gotAudio = filename, string(b)
	return r.result, nil
}
