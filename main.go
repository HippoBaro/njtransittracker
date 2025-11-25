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

	"njtransittracker/njtransit"
	"njtransittracker/server"
)

const (
	defaultPort    = "8080"
	defaultHost    = "0.0.0.0"
	upstreamDomain = "tt.horner.tj"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	etaClient, err := njtransit.NewETAClient(ctx, njtransit.DefaultConfig())
	if err != nil {
		log.Error("failed to create ETA client", "err", err)
		return
	}

	if err = etaClient.Start(); err != nil {
		log.Error("Error creating NJTransit client; no static data will be available", "err", err)
	}

	listenAddr := net.JoinHostPort(defaultHost, defaultPort)
	srv, err := server.NewWebsocketServer(listenAddr, upstreamDomain, etaClient)
	if err != nil {
		log.Error("Failed to create websocket server", "err", err)
		return
	}

	log.Info("Starting websocket server", "addr", listenAddr)
	err = srv.Start(ctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("Websocket server stopped unexpectedly", "err", err)
		return
	}

	log.Info("Websocket server stopped")
}
