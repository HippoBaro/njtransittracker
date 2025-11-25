package main

import (
	"context"
	"errors"
	log "log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"njtransittracker/server"
)

const (
	defaultPort = "8080"
	defaultHost = "0.0.0.0"
	upstreamURL = "https://tt.horner.tj"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listenAddr := net.JoinHostPort(defaultHost, defaultPort)
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
