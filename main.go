package main

import (
	"context"
	"errors"
	log "log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"njtransittracker/server"
)

const (
	listenAddr  = ":8080"
	upstreamURL = "https://tt.horner.tj"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.NewWebsocketServer(listenAddr, upstreamURL)
	if err != nil {
		log.Error("Failed to create websocket server", "err", err)
		os.Exit(1)
	}

	log.Info("Starting websocket server", "addr", listenAddr)
	err = srv.Start(ctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("Websocket server stopped unexpectedly", "err", err)
		os.Exit(1)
	}

	log.Info("Websocket server stopped")
}
