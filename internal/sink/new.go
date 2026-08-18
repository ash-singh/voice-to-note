package sink

import (
	"net/http"
	"time"

	"github.com/ash-singh/voice-to-note/internal/config"
	"github.com/ash-singh/voice-to-note/internal/memo"
)

// sinkTimeout bounds a single delivery attempt to the external tool.
const sinkTimeout = 30 * time.Second

// New builds the sink named by cfg.Sink. cfg is already validated by
// config.Load, so an unknown name falls back to the webhook sink.
func New(cfg config.Config) memo.Sink {
	client := &http.Client{Timeout: sinkTimeout}
	if cfg.Sink == config.SinkNotion {
		return NewNotion("", cfg.NotionToken, cfg.NotionParentPageID, client)
	}
	return NewWebhook(cfg.WebhookURL, client)
}
