package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/thomdehoog/origoa-foundation/internal/httpapi"
	"github.com/thomdehoog/origoa-foundation/internal/repository"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	host := environment("ORIGOA_HOST", "127.0.0.1")
	port, err := strconv.Atoi(environment("ORIGOA_PORT", "3000"))
	if err != nil || port < 1 || port > 65535 {
		return errors.New("ORIGOA_PORT must be between 1 and 65535")
	}
	repositoryPath := environment("ORIGOA_REPOSITORY", filepath.Join(".origoa-data"))
	repo, err := repository.Open(context.Background(), repositoryPath)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(port)),
		Handler:           httpapi.New(repo),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stopped
		if err := httpapi.Shutdown(server, 10*time.Second); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()

	slog.Info("Origoa listening", "address", server.Addr, "repository", repo.Root())
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func environment(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
