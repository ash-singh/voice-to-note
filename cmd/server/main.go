// Command server exposes the voice line API: audio in, LLM-extracted note out,
// note pushed to the configured sink.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ashwanisingh/voiceline-challenge/internal/config"
	"github.com/ashwanisingh/voiceline-challenge/internal/httpapi"
	"github.com/ashwanisingh/voiceline-challenge/internal/llm"
	"github.com/ashwanisingh/voiceline-challenge/internal/logging"
	"github.com/ashwanisingh/voiceline-challenge/internal/sink"
	"github.com/ashwanisingh/voiceline-challenge/internal/voiceline"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(cfg.LogLevel, os.Stdout, slog.String("service", "voiceline"))
	slog.SetDefault(log)

	llmClient := llm.NewClient(llm.Options{
		BaseURL:         cfg.LLMBaseURL,
		APIKey:          cfg.LLMAPIKey,
		TranscribeModel: cfg.TranscribeModel,
		ChatModel:       cfg.ChatModel,
		HTTPClient:      &http.Client{Timeout: cfg.ProcessTimeout},
	})
	noteSink := sink.New(cfg)
	service := voiceline.NewService(llmClient, llmClient, noteSink, log)
	handler := httpapi.NewVoicelineHandler(service, cfg.MaxAudioBytes, cfg.ProcessTimeout, log)

	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewRouter(handler, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		log.Info("server listening", "addr", cfg.Addr, "sink", noteSink.Name(), "chat_model", cfg.ChatModel)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		stop() // restore default signal handling: a second Ctrl-C kills immediately
	}

	log.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}

	log.Info("server stopped")
	return nil
}
