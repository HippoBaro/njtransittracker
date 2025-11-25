package main

import (
	"context"
	log "log/slog"
	"os"
	"os/signal"
	"syscall"

	"njtransittracker/client"
)

func main() {
	const domain = "tt.horner.tj"

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := client.NewScheduleClient(ctx, domain, client.HomeScheduleSubscribe)
	if err := client.Start(); err != nil {
		log.Error("Failed to start schedule client", "err", err)
		return
	}

	messages := client.Schedule()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case msg, ok := <-messages:
			if !ok {
				break loop
			}
			log.Info("Received schedule update", "payload", msg)
		}
	}

	log.Info("Goodbye.")
}
