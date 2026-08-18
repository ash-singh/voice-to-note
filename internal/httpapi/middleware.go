package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ashwanisingh/voiceline-challenge/internal/logging"
)

const requestIDHeader = "X-Request-Id"

// RequestID reuses an inbound request ID or generates one, echoes it back and
// puts it on the request context for correlated logging.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		c.Writer.Header().Set(requestIDHeader, id)
		c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// RequestLogger writes one structured access log line per request.
func RequestLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		attrs := []any{
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"duration_ms", float64(time.Since(start).Microseconds()) / 1000,
			"bytes", c.Writer.Size(),
			"client_ip", c.ClientIP(),
		}
		if err := c.Errors.ByType(gin.ErrorTypePrivate).String(); err != "" {
			attrs = append(attrs, "error", err)
		}
		log.Log(c.Request.Context(), levelFor(status), "http request", attrs...)
	}
}

func levelFor(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// Recovery turns a panic into a logged 500 instead of a dropped connection.
func Recovery(log *slog.Logger) gin.HandlerFunc {
	// io.Discard: the stack goes into the structured log, not to stderr.
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) {
		log.ErrorContext(c.Request.Context(), "panic recovered",
			"panic", recovered, "stack", string(debug.Stack()))
		respondError(c, 500, "internal server error")
	})
}
