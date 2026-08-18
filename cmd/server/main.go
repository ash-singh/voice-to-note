// Command server exposes the voice memo API: audio in, LLM-extracted note out,
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

	"github.com/ash-singh/voice-to-note/internal/config"
	"github.com/ash-singh/voice-to-note/internal/httpapi"
	"github.com/ash-singh/voice-to-note/internal/llm"
	"github.com/ash-singh/voice-to-note/internal/logging"
	"github.com/ash-singh/voice-to-note/internal/memo"
	"github.com/ash-singh/voice-to-note/internal/queue"
	"github.com/ash-singh/voice-to-note/internal/sink"
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

	log := logging.New(cfg.LogLevel, os.Stdout).With("service", "voice-to-note")
	slog.SetDefault(log)

	llmClient := llm.NewClient(llm.Options{
		BaseURL:         cfg.LLMBaseURL,
		APIKey:          cfg.LLMAPIKey,
		TranscribeModel: cfg.TranscribeModel,
		ChatModel:       cfg.ChatModel,
		HTTPClient:      &http.Client{Timeout: cfg.ProcessTimeout},
	})
	noteSink := sink.New(cfg)
	service := memo.NewService(llmClient, llmClient, noteSink, log)
	jobs, err := queue.New(cfg.QueueDir, service, cfg.ProcessTimeout, log)
	if err != nil {
		return err
	}
	handler := httpapi.NewNoteHandler(jobs, cfg.MaxAudioBytes, log)

	// Before any worker starts, so a crashed run's in-flight jobs are resolved
	// rather than raced over.
	if err := jobs.Recover(context.Background()); err != nil {
		return err
	}

	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewRouter(handler, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		jobs.Run(ctx, cfg.QueueWorkers)
	}()

	serverErr := make(chan error, 1)
	go func() {
		log.Info("server listening", "addr", cfg.Addr, "sink", noteSink.Name(),
			"chat_model", cfg.ChatModel, "queue_dir", cfg.QueueDir, "queue_workers", cfg.QueueWorkers)
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
	<-workersDone // the queue is durable, so anything unfinished is picked up on restart

	log.Info("server stopped")
	return nil
}
