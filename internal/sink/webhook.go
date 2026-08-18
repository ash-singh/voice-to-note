// Package sink delivers a finished note to an external system.
package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ash-singh/voiceline-challenge/internal/voiceline"
)

// maxErrBody caps how much of an upstream error body we echo into our error.
const maxErrBody = 512

// checkStatus turns a non-2xx response into an error carrying a snippet of the
// upstream body.
func checkStatus(resp *http.Response, name string) error {
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	return fmt.Errorf("%s returned status %d: %s", name, resp.StatusCode, bytes.TrimSpace(snippet))
}

// Webhook posts the note as JSON to any URL: a Zapier/Make hook feeding Google
// Sheets, a CRM endpoint, or webhook.site for a quick local demo.
type Webhook struct {
	httpClient *http.Client
	url        string
}

func NewWebhook(url string, httpClient *http.Client) *Webhook {
	return &Webhook{httpClient: httpClient, url: url}
}

func (w *Webhook) Name() string { return "webhook" }

// Save posts the note and returns the id the target reported, falling back to
// the target URL when the response carries no id.
func (w *Webhook) Save(ctx context.Context, note voiceline.Note) (string, error) {
	body, err := json.Marshal(note)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if err := checkStatus(resp, "webhook"); err != nil {
		return "", err
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err == nil && out.ID != "" {
		return out.ID, nil
	}
	return w.url, nil
}
