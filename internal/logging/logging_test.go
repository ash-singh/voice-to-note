package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/ash-singh/voice-to-note/internal/logging"
)

func TestNewEmitsJSONWithRequestIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New("debug", &buf).With("service", "voice-to-note")
	ctx := logging.WithRequestID(context.Background(), "req-42")

	log.InfoContext(ctx, "stored", "sink", "webhook")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}
	for k, want := range map[string]any{
		"msg": "stored", "sink": "webhook", "request_id": "req-42", "service": "voice-to-note",
	} {
		if got[k] != want {
			t.Errorf("log[%q] = %v, want %v", k, got[k], want)
		}
	}
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New("warn", &buf)

	log.Info("hidden")
	if buf.Len() != 0 {
		t.Errorf("info line emitted at warn level: %s", buf.String())
	}

	log.Warn("shown")
	if buf.Len() == 0 {
		t.Error("warn line not emitted at warn level")
	}
}
