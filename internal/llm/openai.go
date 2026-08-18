// Package llm talks to an OpenAI-compatible API: speech-to-text for the audio
// and chat completions to turn the transcript into a structured note.
//
// BaseURL is configurable, so any OpenAI-compatible provider (OpenAI, Groq,
// Azure-style gateways, a local stub) works without code changes.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/ash-singh/voiceline-challenge/internal/voiceline"
)

const systemPrompt = `You turn a voice memo transcript into a note for a note-taking tool.
Reply with JSON only, using exactly these keys:
{"title": "<max 8 words>", "summary": "<2-3 sentences>", "action_items": ["<imperative task>"]}
Use the transcript's language. Return an empty action_items array when the memo contains no tasks.`

// maxErrBody caps how much of an upstream error body we echo into our error.
const maxErrBody = 512

// Client is an OpenAI-compatible API client.
type Client struct {
	httpClient      *http.Client
	baseURL         string
	apiKey          string
	transcribeModel string
	chatModel       string
}

// Options configures Client. HTTPClient defaults to http.DefaultClient.
type Options struct {
	BaseURL         string
	APIKey          string
	TranscribeModel string
	ChatModel       string
	HTTPClient      *http.Client
}

func NewClient(o Options) *Client {
	if o.HTTPClient == nil {
		o.HTTPClient = http.DefaultClient
	}
	return &Client{
		httpClient:      o.HTTPClient,
		baseURL:         strings.TrimRight(o.BaseURL, "/"),
		apiKey:          o.APIKey,
		transcribeModel: o.TranscribeModel,
		chatModel:       o.ChatModel,
	}
}

// Transcribe streams the audio to the speech-to-text endpoint and returns the
// recognised text.
func (c *Client) Transcribe(ctx context.Context, filename string, audio io.Reader) (string, error) {
	// The API rejects parts without a known audio extension, so never send an
	// empty filename.
	if filename == "" {
		filename = "audio.m4a"
	}

	// Pipe + multipart writer streams the upload instead of buffering it.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(writeAudioForm(mw, filename, audio, c.transcribeModel))
	}()
	defer pr.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/audio/transcriptions", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	var out struct {
		Text string `json:"text"`
	}
	if err := c.do(req, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}

func writeAudioForm(mw *multipart.Writer, filename string, audio io.Reader, model string) error {
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, audio); err != nil {
		return err
	}
	if err := mw.WriteField("model", model); err != nil {
		return err
	}
	return mw.Close()
}

// Analyze asks the chat model for a structured note built from the transcript.
func (c *Client) Analyze(ctx context.Context, transcript string) (voiceline.Note, error) {
	payload := map[string]any{
		"model": c.chatModel,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": transcript},
		},
		"response_format": map[string]string{"type": "json_object"},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return voiceline.Note{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return voiceline.Note{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := c.do(req, &out); err != nil {
		return voiceline.Note{}, err
	}
	if len(out.Choices) == 0 {
		return voiceline.Note{}, fmt.Errorf("chat completion returned no choices")
	}

	var note voiceline.Note
	if err := json.Unmarshal([]byte(out.Choices[0].Message.Content), &note); err != nil {
		return voiceline.Note{}, fmt.Errorf("model did not return valid JSON: %w", err)
	}
	return note, nil
}

func (c *Client) do(req *http.Request, out any) error {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return fmt.Errorf("%s: unexpected status %d: %s", req.URL.Path, resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
