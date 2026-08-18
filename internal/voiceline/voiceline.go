// Package voiceline holds the domain flow: audio in, structured note out, note
// pushed to an external system.
package voiceline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Note is the structured result the LLM extracts from a spoken voice line.
type Note struct {
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	ActionItems []string `json:"action_items"`
	Transcript  string   `json:"transcript"`
}

// Transcriber turns audio into text (speech-to-text model).
type Transcriber interface {
	Transcribe(ctx context.Context, filename string, audio io.Reader) (string, error)
}

// Analyzer turns a transcript into a structured Note (LLM).
type Analyzer interface {
	Analyze(ctx context.Context, transcript string) (Note, error)
}

// Sink persists a Note in an external system and returns its reference there.
type Sink interface {
	Save(ctx context.Context, note Note) (ref string, err error)
	Name() string
}

// ErrEmptyTranscript means the audio carried no speech worth storing.
var ErrEmptyTranscript = errors.New("transcript is empty")

// Result is what the API returns for one processed voice line.
type Result struct {
	Note    Note   `json:"note"`
	Sink    string `json:"sink"`
	SinkRef string `json:"sink_ref"`
}

// Service wires the three steps together.
type Service struct {
	transcriber Transcriber
	analyzer    Analyzer
	sink        Sink
	log         *slog.Logger
}

func NewService(t Transcriber, a Analyzer, s Sink, log *slog.Logger) *Service {
	return &Service{transcriber: t, analyzer: a, sink: s, log: log}
}

// Process transcribes the audio, extracts a note from it and stores that note
// in the configured sink.
func (s *Service) Process(ctx context.Context, filename string, audio io.Reader) (Result, error) {
	transcript, err := s.transcriber.Transcribe(ctx, filename, audio)
	if err != nil {
		return Result{}, fmt.Errorf("transcribe: %w", err)
	}
	if strings.TrimSpace(transcript) == "" {
		return Result{}, ErrEmptyTranscript
	}
	s.log.DebugContext(ctx, "transcribed audio", "filename", filename, "chars", len(transcript))

	note, err := s.analyzer.Analyze(ctx, transcript)
	if err != nil {
		return Result{}, fmt.Errorf("analyze: %w", err)
	}
	note.Transcript = transcript

	ref, err := s.sink.Save(ctx, note)
	if err != nil {
		return Result{}, fmt.Errorf("save to %s: %w", s.sink.Name(), err)
	}
	s.log.InfoContext(ctx, "voice line stored",
		"sink", s.sink.Name(), "sink_ref", ref, "action_items", len(note.ActionItems))

	return Result{Note: note, Sink: s.sink.Name(), SinkRef: ref}, nil
}
