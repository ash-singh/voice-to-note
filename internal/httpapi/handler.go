// Package httpapi exposes the voice memo flow over HTTP with Gin.
package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ash-singh/voice-to-note/internal/queue"
)

const audioField = "audio"

// allowedAudioExt mirrors the formats the speech-to-text API accepts.
var allowedAudioExt = map[string]bool{
	".flac": true, ".m4a": true, ".mp3": true, ".mp4": true, ".mpeg": true,
	".mpga": true, ".oga": true, ".ogg": true, ".wav": true, ".webm": true,
}

// Jobs is the background queue the handler hands uploads to and reads job state
// back from.
type Jobs interface {
	Enqueue(filename string, audio io.Reader) (string, error)
	Lookup(id string) (queue.Status, error)
}

// NoteHandler serves POST /v1/notes.
type NoteHandler struct {
	queue         Jobs
	maxAudioBytes int64
	log           *slog.Logger
}

func NewNoteHandler(queue Jobs, maxAudioBytes int64, log *slog.Logger) *NoteHandler {
	return &NoteHandler{queue: queue, maxAudioBytes: maxAudioBytes, log: log}
}

// Create accepts a multipart upload with an "audio" file part and queues it for
// transcription, summarising and delivery. The work happens in the background,
// so the response carries a job id rather than the note.
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

	id, err := h.queue.Enqueue(filename, file)
	if err != nil {
		_ = c.Error(err)
		respondError(c, http.StatusInternalServerError, "could not accept the voice memo")
		return
	}

	// The id is a content hash, so this upload may be a duplicate of one that has
	// already been processed. Report where the job really is, not where a fresh
	// one would be.
	state := queue.StateQueued
	if status, err := h.queue.Lookup(id); err == nil {
		state = status.State
	}

	c.Header("Location", "/v1/notes/"+id)
	c.JSON(http.StatusAccepted, gin.H{"data": gin.H{"job_id": id, "state": state}})
}

// Show reports what has become of a queued voice memo.
func (h *NoteHandler) Show(c *gin.Context) {
	status, err := h.queue.Lookup(c.Param("id"))
	if err != nil {
		if errors.Is(err, queue.ErrJobNotFound) {
			respondError(c, http.StatusNotFound, "no such job")
			return
		}
		_ = c.Error(err)
		respondError(c, http.StatusInternalServerError, "could not look up the job")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": status})
}
