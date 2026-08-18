package llm_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ashwanisingh/voiceline-challenge/internal/llm"
)

func newClient(t *testing.T, h http.Handler) *llm.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return llm.NewClient(llm.Options{
		BaseURL:         srv.URL + "/v1/",
		APIKey:          "test-key",
		TranscribeModel: "whisper-1",
		ChatModel:       "gpt-4o-mini",
		HTTPClient:      srv.Client(),
	})
}

func TestTranscribeSendsAudioAsMultipart(t *testing.T) {
	// Arrange
	var gotPath, gotAuth, gotFilename, gotModel, gotAudio string
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		gotModel = r.FormValue("model")
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer file.Close()
		gotFilename = header.Filename
		b, _ := io.ReadAll(file)
		gotAudio = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"text":"call Anna about the invoice"}`)
	}))

	// Act
	got, err := client.Transcribe(context.Background(), "memo.m4a", strings.NewReader("fake-audio"))

	// Assert
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if got != "call Anna about the invoice" {
		t.Errorf("Transcribe() = %q", got)
	}
	if gotPath != "/v1/audio/transcriptions" {
		t.Errorf("path = %q, want /v1/audio/transcriptions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotFilename != "memo.m4a" {
		t.Errorf("upload filename = %q, want memo.m4a (extension is required by the API)", gotFilename)
	}
	if gotModel != "whisper-1" || gotAudio != "fake-audio" {
		t.Errorf("model = %q, audio = %q", gotModel, gotAudio)
	}
}

func TestTranscribeFallsBackToAFilenameWithExtension(t *testing.T) {
	var gotFilename string
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		_, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		gotFilename = header.Filename
		io.WriteString(w, `{"text":"hi"}`)
	}))

	if _, err := client.Transcribe(context.Background(), "", strings.NewReader("x")); err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if !strings.Contains(gotFilename, ".") {
		t.Errorf("upload filename = %q, want a name with an extension", gotFilename)
	}
}

func TestAnalyzeParsesStructuredNote(t *testing.T) {
	var gotPath, gotBody string
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"title\":\"Invoice\",\"summary\":\"Call Anna.\",\"action_items\":[\"Call Anna\"]}"}}]}`)
	}))

	note, err := client.Analyze(context.Background(), "call Anna about the invoice")
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if note.Title != "Invoice" || note.Summary != "Call Anna." || len(note.ActionItems) != 1 {
		t.Errorf("Analyze() = %+v", note)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	for _, want := range []string{"gpt-4o-mini", "json_object", "call Anna about the invoice"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %q: %s", want, gotBody)
		}
	}
}

func TestAnalyzeErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantMsg string
	}{
		{
			name: "upstream error status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, `{"error":"rate limited"}`)
			},
			wantMsg: "unexpected status 429",
		},
		{
			name: "no choices",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, `{"choices":[]}`)
			},
			wantMsg: "no choices",
		},
		{
			name: "content is not json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, `{"choices":[{"message":{"content":"sorry, I cannot"}}]}`)
			},
			wantMsg: "did not return valid JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClient(t, tt.handler)

			_, err := client.Analyze(context.Background(), "hello")

			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Analyze() error = %v, want containing %q", err, tt.wantMsg)
			}
		})
	}
}
