// Package logging builds the application's structured slog logger.
//
// Everything is emitted as JSON on the configured writer (stdout in the server)
// so a log shipper — Axiom's collector, Vector, the Docker log driver — can
// ingest it without any parsing rules.
package logging

import (
	"context"
	"io"
	"log/slog"
)

// New returns a JSON logger at the given level ("debug", "info", "warn",
// "error"). Unknown levels fall back to info.
func New(level string, w io.Writer, attrs ...slog.Attr) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(&contextHandler{Handler: h.WithAttrs(attrs)})
}

type requestIDKey struct{}

// WithRequestID stores a request ID on the context so every log record made
// with that context is correlated to the request.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request ID stored on ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// contextHandler copies the context request ID onto every record.
type contextHandler struct{ slog.Handler }

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestIDFrom(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
