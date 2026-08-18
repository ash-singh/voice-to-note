package sink

import (
	"net/http"
	"time"

	"github.com/ash-singh/voiceline-challenge/internal/config"
	"github.com/ash-singh/voiceline-challenge/internal/voiceline"
)

// sinkTimeout bounds a single delivery attempt to the external tool.
const sinkTimeout = 30 * time.Second

// New builds the sink named by cfg.Sink. cfg is already validated by
// config.Load, so an unknown name falls back to the webhook sink.
func New(cfg config.Config) voiceline.Sink {
	client := &http.Client{Timeout: sinkTimeout}
	if cfg.Sink == config.SinkNotion {
		return NewNotion(NotionOptions{
			BaseURL:      cfg.NotionBaseURL,
			Token:        cfg.NotionToken,
			ParentPageID: cfg.NotionParentPageID,
			HTTPClient:   client,
		})
	}
	return NewWebhook(cfg.WebhookURL, client)
}
