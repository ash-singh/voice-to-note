// Package httpapi exposes the voice memo flow over HTTP with Gin.
package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ash-singh/voice-to-note/internal/memo"
)

const audioField = "audio"

// allowedAudioExt mirrors the formats the speech-to-text API accepts.
var allowedAudioExt = map[string]bool{
	".flac": true, ".m4a": true, ".mp3": true, ".mp4": true, ".mpeg": true,
	".mpga": true, ".oga": true, ".ogg": true, ".wav": true, ".webm": true,
}

// Processor is the domain entry point the handler depends on.
type Processor interface {
	Process(ctx context.Context, filename string, audio io.Reader) (memo.Result, error)
}

// NoteHandler serves POST /v1/notes.
type NoteHandler struct {
	svc           Processor
	maxAudioBytes int64
	timeout       time.Duration
	log           *slog.Logger
}

func NewNoteHandler(svc Processor, maxAudioBytes int64, timeout time.Duration, log *slog.Logger) *NoteHandler {
	return &NoteHandler{svc: svc, maxAudioBytes: maxAudioBytes, timeout: timeout, log: log}
}

// Create accepts a multipart upload with an "audio" file part, has it
// transcribed and summarised, and stores the note in the configured sink.
func (h *NoteHandler) Create(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxAudioBytes)

	fileHeader, err := c.FormFile(audioField)
	switch {
	case errors.Is(err, http.ErrMissingFile):
		respondError(c, http.StatusBadRequest, "multipart field \"audio\" is required")
		return
	case err != nil:
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respondError(c, http.StatusRequestEntityTooLarge, "audio exceeds the size limit")
			return
		}
		respondError(c, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}

	filename := filepath.Base(fileHeader.Filename)
	if !allowedAudioExt[strings.ToLower(filepath.Ext(filename))] {
		respondError(c, http.StatusUnsupportedMediaType, "unsupported audio format: "+filepath.Ext(filename))
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "cannot open upload", "error", err)
		respondError(c, http.StatusInternalServerError, "cannot read uploaded audio")
		return
	}
	defer file.Close()

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()

	result, err := h.svc.Process(ctx, filename, file)
	switch {
	case errors.Is(err, memo.ErrEmptyTranscript):
		respondError(c, http.StatusUnprocessableEntity, "no speech detected in the audio")
		return
	case err != nil:
		_ = c.Error(err)
		respondError(c, http.StatusBadGateway, "could not process the voice memo")
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": result})
}
