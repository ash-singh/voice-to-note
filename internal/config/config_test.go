package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ash-singh/voiceline-challenge/internal/config"
)

// configEnv is every variable Load reads; tests clear them so the developer's
// own shell cannot influence the result.
var configEnv = []string{
	"ADDR", "LOG_LEVEL", "MAX_AUDIO_BYTES", "PROCESS_TIMEOUT", "SHUTDOWN_TIMEOUT",
	"LLM_BASE_URL", "LLM_API_KEY", "TRANSCRIBE_MODEL", "CHAT_MODEL",
	"SINK", "WEBHOOK_URL", "NOTION_TOKEN", "NOTION_PARENT_PAGE_ID",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range configEnv {
		if old, ok := os.LookupEnv(key); ok {
			t.Cleanup(func() { os.Setenv(key, old) })
			os.Unsetenv(key)
		}
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("LLM_API_KEY", "key")
	t.Setenv("WEBHOOK_URL", "https://sink.example/hook")

	cfg, err := config.Load()

	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != ":8080" || cfg.LogLevel != "info" || cfg.Sink != config.SinkWebhook {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.MaxAudioBytes != 25<<20 {
		t.Errorf("MaxAudioBytes = %d, want %d", cfg.MaxAudioBytes, 25<<20)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.LLMBaseURL != "https://api.openai.com/v1" {
		t.Errorf("LLMBaseURL = %q", cfg.LLMBaseURL)
	}
}

func TestLoadOverridesFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("LLM_API_KEY", "key")
	t.Setenv("SINK", "NOTION")
	t.Setenv("NOTION_TOKEN", "secret")
	t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
	t.Setenv("ADDR", ":9000")
	t.Setenv("MAX_AUDIO_BYTES", "1024")
	t.Setenv("PROCESS_TIMEOUT", "30s")

	cfg, err := config.Load()

	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Sink != config.SinkNotion {
		t.Errorf("Sink = %q, want notion (case-insensitive)", cfg.Sink)
	}
	if cfg.Addr != ":9000" || cfg.MaxAudioBytes != 1024 || cfg.ProcessTimeout != 30*time.Second {
		t.Errorf("overrides = %+v", cfg)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantMsg string
	}{
		{
			name:    "missing api key",
			env:     map[string]string{"WEBHOOK_URL": "https://sink.example"},
			wantMsg: "LLM_API_KEY is required",
		},
		{
			name:    "webhook sink without url",
			env:     map[string]string{"LLM_API_KEY": "key"},
			wantMsg: "WEBHOOK_URL is required",
		},
		{
			name:    "notion sink without token",
			env:     map[string]string{"LLM_API_KEY": "key", "SINK": "notion", "NOTION_PARENT_PAGE_ID": "p"},
			wantMsg: "NOTION_TOKEN is required",
		},
		{
			name:    "unknown sink",
			env:     map[string]string{"LLM_API_KEY": "key", "SINK": "carrier-pigeon"},
			wantMsg: `unknown SINK "carrier-pigeon"`,
		},
		{
			name:    "unparseable duration",
			env:     map[string]string{"LLM_API_KEY": "key", "WEBHOOK_URL": "https://x", "PROCESS_TIMEOUT": "soon"},
			wantMsg: "PROCESS_TIMEOUT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := config.Load()

			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.wantMsg)
			}
		})
	}
}
