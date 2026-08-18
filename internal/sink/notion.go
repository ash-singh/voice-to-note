package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ash-singh/voice-to-note/internal/memo"
)

const (
	notionBaseURL = "https://api.notion.com/v1"
	notionVersion = "2022-06-28"
	// notionTextLimit is Notion's per-rich-text-object character limit.
	notionTextLimit = 2000
)

// Notion creates one page per voice memo under a parent page. A page parent (as
// opposed to a database) keeps the payload free of database schema coupling.
type Notion struct {
	httpClient   *http.Client
	baseURL      string
	token        string
	parentPageID string
}

// NewNotion returns a sink creating pages under parentPageID. baseURL is
// overridden only by tests; pass "" for the Notion API.
func NewNotion(baseURL, token, parentPageID string, httpClient *http.Client) *Notion {
	if baseURL == "" {
		baseURL = notionBaseURL
	}
	return &Notion{
		httpClient:   httpClient,
		baseURL:      strings.TrimRight(baseURL, "/"),
		token:        token,
		parentPageID: parentPageID,
	}
}

func (n *Notion) Name() string { return "notion" }

// Save creates the Notion page and returns its URL.
func (n *Notion) Save(ctx context.Context, note memo.Note) (string, error) {
	body, err := json.Marshal(map[string]any{
		"parent": map[string]string{"page_id": n.parentPageID},
		"properties": map[string]any{
			"title": map[string]any{"title": richText(note.Title)},
		},
		"children": notionChildren(note),
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.baseURL+"/pages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Notion-Version", notionVersion)

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if err := checkStatus(resp, "notion"); err != nil {
		return "", err
	}

	var out struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode notion response: %w", err)
	}
	return out.URL, nil
}

func notionChildren(note memo.Note) []map[string]any {
	children := []map[string]any{paragraph(note.Summary)}
	for _, item := range note.ActionItems {
		children = append(children, map[string]any{
			"object": "block",
			"type":   "to_do",
			"to_do":  map[string]any{"rich_text": richText(item), "checked": false},
		})
	}
	children = append(children, paragraph("Transcript: "+note.Transcript))
	return children
}

func paragraph(text string) map[string]any {
	return map[string]any{
		"object":    "block",
		"type":      "paragraph",
		"paragraph": map[string]any{"rich_text": richText(text)},
	}
}

// richText splits text into Notion rich text objects, respecting the 2000
// character limit per object.
func richText(text string) []map[string]any {
	out := make([]map[string]any, 0, len(text)/notionTextLimit+1)
	for len(text) > 0 {
		end := min(notionTextLimit, len(text))
		// Do not split inside a multi-byte rune.
		for end < len(text) && text[end]&0xC0 == 0x80 {
			end--
		}
		out = append(out, map[string]any{"type": "text", "text": map[string]string{"content": text[:end]}})
		text = text[end:]
	}
	return out
}
