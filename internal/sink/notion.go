package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ash-singh/voiceline-challenge/internal/voiceline"
)

const (
	notionVersion = "2022-06-28"
	// notionTextLimit is Notion's per-rich-text-object character limit.
	notionTextLimit = 2000
)

// Notion creates one page per voice line under a parent page. A page parent (as
// opposed to a database) keeps the payload free of database schema coupling.
type Notion struct {
	httpClient   *http.Client
	baseURL      string
	token        string
	parentPageID string
}

type NotionOptions struct {
	BaseURL      string
	Token        string
	ParentPageID string
	HTTPClient   *http.Client
}

func NewNotion(o NotionOptions) *Notion {
	if o.HTTPClient == nil {
		o.HTTPClient = http.DefaultClient
	}
	return &Notion{
		httpClient:   o.HTTPClient,
		baseURL:      strings.TrimRight(o.BaseURL, "/"),
		token:        o.Token,
		parentPageID: o.ParentPageID,
	}
}

func (n *Notion) Name() string { return "notion" }

// Save creates the Notion page and returns its URL.
func (n *Notion) Save(ctx context.Context, note voiceline.Note) (string, error) {
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

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return "", fmt.Errorf("notion returned status %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode notion response: %w", err)
	}
	if out.URL != "" {
		return out.URL, nil
	}
	return out.ID, nil
}

func notionChildren(note voiceline.Note) []map[string]any {
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
