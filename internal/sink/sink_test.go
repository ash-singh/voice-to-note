package sink_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ashwanisingh/voiceline-challenge/internal/sink"
	"github.com/ashwanisingh/voiceline-challenge/internal/voiceline"
)

var testNote = voiceline.Note{
	Title:       "Invoice",
	Summary:     "Call Anna.",
	ActionItems: []string{"Call Anna", "Send the invoice"},
	Transcript:  "call Anna about the invoice",
}

func TestWebhookSavePostsNoteAsJSON(t *testing.T) {
	// Arrange
	var gotBody voiceline.Note
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		io.WriteString(w, `{"id":"row-7"}`)
	}))
	defer srv.Close()
	s := sink.NewWebhook(srv.URL, srv.Client())

	// Act
	ref, err := s.Save(context.Background(), testNote)

	// Assert
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if ref != "row-7" {
		t.Errorf("Save() ref = %q, want row-7", ref)
	}
	if s.Name() != "webhook" {
		t.Errorf("Name() = %q", s.Name())
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotBody.Title != "Invoice" || len(gotBody.ActionItems) != 2 {
		t.Errorf("posted note = %+v", gotBody)
	}
}

func TestWebhookSaveFallsBackToURLWithoutID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	ref, err := sink.NewWebhook(srv.URL, srv.Client()).Save(context.Background(), testNote)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if ref != srv.URL {
		t.Errorf("Save() ref = %q, want %q", ref, srv.URL)
	}
}

func TestWebhookSaveReportsUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "nope")
	}))
	defer srv.Close()

	_, err := sink.NewWebhook(srv.URL, srv.Client()).Save(context.Background(), testNote)
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("Save() error = %v, want status 500", err)
	}
}

func TestNotionSaveCreatesPageUnderParent(t *testing.T) {
	// Arrange
	var gotPath, gotAuth, gotVersion string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("Notion-Version")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		io.WriteString(w, `{"id":"page-1","url":"https://notion.so/page-1"}`)
	}))
	defer srv.Close()
	s := sink.NewNotion(sink.NotionOptions{
		BaseURL: srv.URL + "/v1", Token: "secret", ParentPageID: "parent-1", HTTPClient: srv.Client(),
	})

	// Act
	ref, err := s.Save(context.Background(), testNote)

	// Assert
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if ref != "https://notion.so/page-1" {
		t.Errorf("Save() ref = %q", ref)
	}
	if gotPath != "/v1/pages" || gotAuth != "Bearer secret" || gotVersion == "" {
		t.Errorf("request = %s, auth %q, version %q", gotPath, gotAuth, gotVersion)
	}
	parent, _ := gotBody["parent"].(map[string]any)
	if parent["page_id"] != "parent-1" {
		t.Errorf("parent = %v, want page_id parent-1", gotBody["parent"])
	}
	children, _ := gotBody["children"].([]any)
	if len(children) != 4 { // summary + 2 action items + transcript
		t.Fatalf("children = %d, want 4", len(children))
	}
	if got := children[1].(map[string]any)["type"]; got != "to_do" {
		t.Errorf("action item block type = %v, want to_do", got)
	}
}

func TestNotionSaveChunksLongTranscript(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{"id":"page-1","url":"https://notion.so/page-1"}`)
	}))
	defer srv.Close()
	s := sink.NewNotion(sink.NotionOptions{BaseURL: srv.URL, Token: "t", ParentPageID: "p", HTTPClient: srv.Client()})

	long := voiceline.Note{Title: "Long", Summary: "s", Transcript: strings.Repeat("ä", 3000)}
	if _, err := s.Save(context.Background(), long); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	children := gotBody["children"].([]any)
	transcriptBlock := children[len(children)-1].(map[string]any)
	richText := transcriptBlock["paragraph"].(map[string]any)["rich_text"].([]any)
	if len(richText) < 2 {
		t.Fatalf("rich_text objects = %d, want the transcript split into chunks", len(richText))
	}
	for i, rt := range richText {
		content := rt.(map[string]any)["text"].(map[string]any)["content"].(string)
		if len(content) > 2000 {
			t.Errorf("chunk %d is %d bytes, over Notion's 2000 limit", i, len(content))
		}
	}
}

func TestNotionSaveReportsUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"API token is invalid"}`)
	}))
	defer srv.Close()

	_, err := sink.NewNotion(sink.NotionOptions{BaseURL: srv.URL, Token: "bad", HTTPClient: srv.Client()}).
		Save(context.Background(), testNote)
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("Save() error = %v, want status 401", err)
	}
}
