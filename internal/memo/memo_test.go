package memo_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ash-singh/voice-to-note/internal/memo"
)

type fakeTranscriber struct {
	text     string
	err      error
	gotName  string
	gotAudio string
}

func (f *fakeTranscriber) Transcribe(_ context.Context, filename string, audio io.Reader) (string, error) {
	b, _ := io.ReadAll(audio)
	f.gotName, f.gotAudio = filename, string(b)
	return f.text, f.err
}

type fakeAnalyzer struct {
	note          memo.Note
	err           error
	gotTranscript string
}

func (f *fakeAnalyzer) Analyze(_ context.Context, transcript string) (memo.Note, error) {
	f.gotTranscript = transcript
	return f.note, f.err
}

type fakeSink struct {
	ref     string
	err     error
	gotNote memo.Note
}

func (f *fakeSink) Save(_ context.Context, note memo.Note) (string, error) {
	f.gotNote = note
	return f.ref, f.err
}

func (f *fakeSink) Name() string { return "fake" }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestProcessStoresExtractedNote(t *testing.T) {
	// Arrange
	tr := &fakeTranscriber{text: "call Anna about the invoice"}
	an := &fakeAnalyzer{note: memo.Note{
		Title:       "Invoice follow-up",
		Summary:     "Needs a call with Anna.",
		ActionItems: []string{"Call Anna"},
	}}
	sk := &fakeSink{ref: "page-1"}
	svc := memo.NewService(tr, an, sk, discardLogger())

	// Act
	got, err := svc.Process(context.Background(), "memo.m4a", strings.NewReader("RIFF"))

	// Assert
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if tr.gotName != "memo.m4a" || tr.gotAudio != "RIFF" {
		t.Errorf("transcriber got (%q, %q), want (memo.m4a, RIFF)", tr.gotName, tr.gotAudio)
	}
	if an.gotTranscript != "call Anna about the invoice" {
		t.Errorf("analyzer got transcript %q", an.gotTranscript)
	}
	if sk.gotNote.Transcript != "call Anna about the invoice" {
		t.Errorf("sink note missing transcript, got %q", sk.gotNote.Transcript)
	}
	if got.SinkRef != "page-1" || got.Sink != "fake" {
		t.Errorf("Process() = %+v, want sink fake/page-1", got)
	}
	if got.Note.Title != "Invoice follow-up" {
		t.Errorf("Process() title = %q", got.Note.Title)
	}
}

func TestProcessErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		tr      *fakeTranscriber
		an      *fakeAnalyzer
		sk      *fakeSink
		wantErr error
		wantMsg string
	}{
		{
			name:    "transcriber fails",
			tr:      &fakeTranscriber{err: boom},
			an:      &fakeAnalyzer{},
			sk:      &fakeSink{},
			wantErr: boom,
			wantMsg: "transcribe:",
		},
		{
			name:    "silent audio",
			tr:      &fakeTranscriber{text: "   "},
			an:      &fakeAnalyzer{},
			sk:      &fakeSink{},
			wantErr: memo.ErrEmptyTranscript,
		},
		{
			name:    "analyzer fails",
			tr:      &fakeTranscriber{text: "hello"},
			an:      &fakeAnalyzer{err: boom},
			sk:      &fakeSink{},
			wantErr: boom,
			wantMsg: "analyze:",
		},
		{
			name:    "sink fails",
			tr:      &fakeTranscriber{text: "hello"},
			an:      &fakeAnalyzer{},
			sk:      &fakeSink{err: boom},
			wantErr: boom,
			wantMsg: "save to fake:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := memo.NewService(tt.tr, tt.an, tt.sk, discardLogger())

			_, err := svc.Process(context.Background(), "memo.m4a", strings.NewReader("x"))

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Process() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Process() error = %q, want prefix %q", err, tt.wantMsg)
			}
		})
	}
}

func TestProcessSkipsSinkOnEmptyTranscript(t *testing.T) {
	sk := &fakeSink{}
	svc := memo.NewService(&fakeTranscriber{text: ""}, &fakeAnalyzer{}, sk, discardLogger())

	if _, err := svc.Process(context.Background(), "memo.m4a", strings.NewReader("x")); err == nil {
		t.Fatal("Process() error = nil, want ErrEmptyTranscript")
	}
	if sk.gotNote.Transcript != "" {
		t.Error("sink was called for an empty transcript")
	}
}
