package main

import (
	"context"
	"fmt"
	log "log/slog"
	"os"
	"os/signal"
	"syscall"

	"njtransittracker/model"
	"njtransittracker/njtransit"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	etaClient, err := njtransit.NewETAClient(ctx, njtransit.DefaultConfig())
	if err != nil {
		log.Error("failed to create ETA client", "err", err)
		return
	}

	update, finalize := etaClient.Notify()
	defer finalize()

	err = etaClient.Start()
	if err != nil {
		log.Error("failed to start ETA client", "err", err)
		return
	}

	etaClient.Track("158", "21852")
	etaClient.Track("159", "21852")
	etaClient.Track("156", "21852")
	etaClient.Track("HBLR", "PORT IMP")

	var etas model.Trips
	for {
		select {
		case err = <-update:
			if err != nil {
				log.Error("failed to start ETA client", "err", err)
			}

			newETAs := etaClient.GetNextETAs("158", "21852")
			if !newETAs.Equal(etas) {
				fmt.Println(newETAs)
			}
			etas = newETAs
		case <-ctx.Done():
			log.Info("Goodbye.")
			return
		}

	}
}
