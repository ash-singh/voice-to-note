// Package config loads the server configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sink names.
const (
	SinkWebhook = "webhook"
	SinkNotion  = "notion"
)

// Config holds everything the server needs to run.
type Config struct {
	Addr            string
	LogLevel        string
	MaxAudioBytes   int64
	ProcessTimeout  time.Duration
	ShutdownTimeout time.Duration

	LLMBaseURL      string
	LLMAPIKey       string
	TranscribeModel string
	ChatModel       string

	QueueDir      string
	QueueWorkers  int
	QueueMaxDepth int

	Sink               string
	WebhookURL         string
	NotionToken        string
	NotionParentPageID string
}

// Load reads the configuration from the environment and validates it.
func Load() (Config, error) {
	maxBytes, err := strconv.ParseInt(env("MAX_AUDIO_BYTES", "26214400"), 10, 64) // 25 MiB, the OpenAI limit
	if err != nil {
		return Config{}, fmt.Errorf("MAX_AUDIO_BYTES: %w", err)
	}
	processTimeout, err := time.ParseDuration(env("PROCESS_TIMEOUT", "120s"))
	if err != nil {
		return Config{}, fmt.Errorf("PROCESS_TIMEOUT: %w", err)
	}
	shutdownTimeout, err := time.ParseDuration(env("SHUTDOWN_TIMEOUT", "10s"))
	if err != nil {
		return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
	}
	queueWorkers, err := strconv.Atoi(env("QUEUE_WORKERS", "2"))
	if err != nil {
		return Config{}, fmt.Errorf("QUEUE_WORKERS: %w", err)
	}
	queueMaxDepth, err := strconv.Atoi(env("QUEUE_MAX_DEPTH", "100"))
	if err != nil {
		return Config{}, fmt.Errorf("QUEUE_MAX_DEPTH: %w", err)
	}

	cfg := Config{
		Addr:            env("ADDR", ":8080"),
		LogLevel:        env("LOG_LEVEL", "info"),
		MaxAudioBytes:   maxBytes,
		ProcessTimeout:  processTimeout,
		ShutdownTimeout: shutdownTimeout,

		LLMBaseURL:      env("LLM_BASE_URL", "https://api.openai.com/v1"),
		LLMAPIKey:       os.Getenv("LLM_API_KEY"),
		TranscribeModel: env("TRANSCRIBE_MODEL", "whisper-1"),
		ChatModel:       env("CHAT_MODEL", "gpt-4o-mini"),

		QueueDir:      env("QUEUE_DIR", "queue"),
		QueueWorkers:  queueWorkers,
		QueueMaxDepth: queueMaxDepth,

		Sink:               strings.ToLower(env("SINK", SinkWebhook)),
		WebhookURL:         os.Getenv("WEBHOOK_URL"),
		NotionToken:        os.Getenv("NOTION_TOKEN"),
		NotionParentPageID: os.Getenv("NOTION_PARENT_PAGE_ID"),
	}

	return cfg, cfg.validate()
}

func (c Config) validate() error {
	var errs []error
	if c.LLMAPIKey == "" {
		errs = append(errs, errors.New("LLM_API_KEY is required"))
	}
	if c.MaxAudioBytes <= 0 {
		errs = append(errs, errors.New("MAX_AUDIO_BYTES must be positive"))
	}
	if c.QueueWorkers <= 0 {
		errs = append(errs, errors.New("QUEUE_WORKERS must be positive"))
	}
	switch c.Sink {
	case SinkWebhook:
		if c.WebhookURL == "" {
			errs = append(errs, errors.New("WEBHOOK_URL is required when SINK=webhook"))
		}
	case SinkNotion:
		if c.NotionToken == "" {
			errs = append(errs, errors.New("NOTION_TOKEN is required when SINK=notion"))
		}
		if c.NotionParentPageID == "" {
			errs = append(errs, errors.New("NOTION_PARENT_PAGE_ID is required when SINK=notion"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown SINK %q, want %q or %q", c.Sink, SinkWebhook, SinkNotion))
	}
	return errors.Join(errs...)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
