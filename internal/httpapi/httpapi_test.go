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

	"github.com/ashwanisingh/voiceline-challenge/internal/httpapi"
	"github.com/ashwanisingh/voiceline-challenge/internal/logging"
	"github.com/ashwanisingh/voiceline-challenge/internal/voiceline"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

type fakeProcessor struct {
	result   voiceline.Result
	err      error
	gotName  string
	gotAudio string
}

func (f *fakeProcessor) Process(_ context.Context, filename string, audio io.Reader) (voiceline.Result, error) {
	b, _ := io.ReadAll(audio)
	f.gotName, f.gotAudio = filename, string(b)
	return f.result, f.err
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

func newServer(t *testing.T, p httpapi.Processor, maxBytes int64) (*gin.Engine, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log := logging.New("debug", &logs)
	return httpapi.NewRouter(httpapi.NewVoicelineHandler(p, maxBytes, time.Second, log), log), &logs
}

func TestCreateVoicelineReturnsStoredNote(t *testing.T) {
	// Arrange
	proc := &fakeProcessor{result: voiceline.Result{
		Note:    voiceline.Note{Title: "Invoice", Summary: "Call Anna", ActionItems: []string{"Call Anna"}},
		Sink:    "webhook",
		SinkRef: "https://sink.example/1",
	}}
	router, logs := newServer(t, proc, 1<<20)
	body, contentType := audioBody(t, "audio", "memo.m4a", "fake-audio")
	req := httptest.NewRequest(http.MethodPost, "/v1/voicelines", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()

	// Act
	router.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	var got struct{ Data voiceline.Result }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.Data.SinkRef != "https://sink.example/1" || got.Data.Note.Title != "Invoice" {
		t.Errorf("response data = %+v", got.Data)
	}
	if proc.gotName != "memo.m4a" || proc.gotAudio != "fake-audio" {
		t.Errorf("processor got (%q, %q)", proc.gotName, proc.gotAudio)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("X-Request-Id response header is empty")
	}
	if !strings.Contains(logs.String(), `"msg":"http request"`) {
		t.Errorf("no access log line emitted: %s", logs.String())
	}
}

func TestCreateVoicelineRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name       string
		field      string
		filename   string
		content    string
		maxBytes   int64
		procErr    error
		wantStatus int
	}{
		{name: "missing audio field", field: "file", filename: "memo.m4a", content: "x", maxBytes: 1 << 20, wantStatus: http.StatusBadRequest},
		{name: "unsupported format", field: "audio", filename: "memo.txt", content: "x", maxBytes: 1 << 20, wantStatus: http.StatusUnsupportedMediaType},
		{name: "audio too large", field: "audio", filename: "memo.m4a", content: strings.Repeat("x", 2048), maxBytes: 512, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "silent audio", field: "audio", filename: "memo.m4a", content: "x", maxBytes: 1 << 20, procErr: voiceline.ErrEmptyTranscript, wantStatus: http.StatusUnprocessableEntity},
		{name: "upstream failure", field: "audio", filename: "memo.m4a", content: "x", maxBytes: 1 << 20, procErr: errors.New("llm down"), wantStatus: http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, _ := newServer(t, &fakeProcessor{err: tt.procErr}, tt.maxBytes)
			body, contentType := audioBody(t, tt.field, tt.filename, tt.content)
			req := httptest.NewRequest(http.MethodPost, "/v1/voicelines", body)
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
	router, logs := newServer(t, &fakeProcessor{}, 1<<20)
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
