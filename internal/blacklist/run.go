package blacklist

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Main is the shared entry point used by both ./main.go and
// ./cmd/server/main.go. It loads configuration, opens the database, starts the
// HTTP server and shuts down gracefully on SIGINT/SIGTERM.
func Main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("close database: %v", err)
		}
	}()

	if err := store.Init(context.Background()); err != nil {
		log.Fatalf("init database: %v", err)
	}

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           NewServer(store, cfg).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("EH blacklist API listening on %s", cfg.Addr)
	log.Printf("DB path: %s", cfg.DBPath)
	if cfg.APIToken == "" {
		log.Printf("auth: disabled (set API_TOKEN to require a bearer token)")
	} else {
		log.Printf("auth: bearer token required")
	}
	if len(cfg.CORSOrigins) == 0 {
		log.Printf("cors: *")
	} else {
		log.Printf("cors: %s", strings.Join(cfg.CORSOrigins, ", "))
	}
	log.Printf("limits: max_check_items=%d max_body_bytes=%d", cfg.MaxCheckItems, cfg.MaxBodyBytes)
	if cfg.TrustProxy {
		log.Printf("proxy: trusting X-Forwarded-For / X-Real-IP")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
		}
		return
	case <-ctx.Done():
		log.Printf("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}
