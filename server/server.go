package server

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"

	"njtransittracker/model"
	"njtransittracker/njtransit"

	"golang.org/x/net/websocket"
)

const (
	initialMessageTimeout  = 30 * time.Second
	heartbeatInterval      = 10 * time.Second
	websocketWriteDeadline = 5 * time.Second
	serverShutdownTimeout  = 5 * time.Second
)

type WebsocketServer struct {
	addr           string
	upstreamDomain string
	proxy          *httputil.ReverseProxy
	etaClient      *njtransit.ETAClient
}

func NewWebsocketServer(addr, upstreamDomain string, etaClient *njtransit.ETAClient) (*WebsocketServer, error) {
	upstream, err := url.Parse(fmt.Sprintf("https://%s", upstreamDomain))
	if err != nil {
		return nil, fmt.Errorf("parse proxy target %q: %w", upstreamDomain, err)
	}

	return &WebsocketServer{
		addr:           addr,
		upstreamDomain: upstreamDomain,
		proxy:          httputil.NewSingleHostReverseProxy(upstream),
		etaClient:      etaClient,
	}, nil
}

func (s *WebsocketServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	wsHandler := websocket.Handler(func(conn *websocket.Conn) {
		s.handleWebsocketConnection(ctx, conn)
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !s.etaClient.UpToDate() {
			http.Error(w, fmt.Sprintf("stale schedule data"), http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isWebsocketRequest(r) {
			wsHandler.ServeHTTP(w, r)
			return
		}

		log.Info("Proxying HTTP request to upstream", "endpoint", s.upstreamDomain, "request", r.RequestURI)
		s.proxy.ServeHTTP(w, r)
	})

	httpServer := &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	errCh := make(chan error, 1)
	go func() {
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("Failed to shut down websocket server", "err", err)
			return err
		}

		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func (s *WebsocketServer) handleWebsocketConnection(ctx context.Context, conn *websocket.Conn) {
	defer conn.Close()
	conn.PayloadType = websocket.TextFrame

	message, err := waitForInitialMessage(conn)
	if err != nil {
		log.Warn("Closing connection: failed to receive initial message", "err", err, "remote", conn.Request().RemoteAddr)
		return
	}

	log.Info("New client connected", "remote", conn.Request().RemoteAddr, "message", message)

	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	notifyBus, finalize := s.etaClient.Notify()
	defer finalize()

	s.etaClient.Track("158", "21852")
	s.etaClient.Track("159", "21852")
	s.etaClient.Track("156", "21852")
	s.etaClient.Track("HBLR", "PORT IMP")

	// Pull initial data from the client to avoid waiting for the next refresh, if any.
	s.etaClient.Cached(time.Now().Add(-30*time.Second), notifyBus)

	var etas model.Trips
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeatTicker.C:
			if err = send(conn, model.Heartbeat{Event: model.EventHeartBeat}); err != nil {
				log.Warn("Failed to send heartbeat, closing connection", "err", err, "remote", conn.Request().RemoteAddr)
				return
			}

			// Wait until either data source wakes us up
		case err = <-notifyBus:
			if err != nil {
				log.Warn("Failed to retrieve schedule changes for subscribed routes", "err", err, "remote", conn.Request().RemoteAddr)
				continue
			}

			busETAs := slices.Concat(
				s.etaClient.GetNextETAs("156", "21852"),
				s.etaClient.GetNextETAs("158", "21852"),
				s.etaClient.GetNextETAs("159", "21852"),
			).Sort().Trim(2)
			trainETA := s.etaClient.GetNextETAs("HBLR", "PORT IMP").Filter("WEST SIDE").WithRouteName("HBLR").Sort().Trim(3 - len(busETAs))
			combined := slices.Concat(busETAs, trainETA).Sort()

			if etas.Equal(combined) {
				// No changes detected since we last pushed an ETA update

				continue
			}

			schedMessage := model.ScheduleChange{
				Event: model.EventSchedule,
				Data:  model.ScheduleData{Trips: combined},
			}

			log.Info("sending schedule", "remote", conn.Request().RemoteAddr, "schedule", schedMessage)
			if err = send(conn, schedMessage); err != nil {
				log.Warn("Failed to send schedule change, closing connection", "err", err, "remote", conn.Request().RemoteAddr)
				return
			}
			etas = combined
		}
	}
}

func send[T any](conn *websocket.Conn, message model.Event[T]) (err error) {
	if err = conn.SetWriteDeadline(time.Now().Add(websocketWriteDeadline)); err != nil {
		log.Warn("Closing connection: failed to set connection property", "err", err, "remote", conn.Request().RemoteAddr)
		return
	}

	return websocket.JSON.Send(conn, message)
}

func waitForInitialMessage(conn *websocket.Conn) (model.SubscribeRequest, error) {
	if err := conn.SetReadDeadline(time.Now().Add(initialMessageTimeout)); err != nil {
		return model.SubscribeRequest{}, err
	}
	defer conn.SetReadDeadline(time.Time{})

	var raw []byte
	if err := websocket.Message.Receive(conn, &raw); err != nil {
		return model.SubscribeRequest{}, fmt.Errorf("remote failed to send message within intial window: %w", err)
	}

	msg, err := model.FromRaw(raw)
	if err != nil {
		return model.SubscribeRequest{}, err
	} else if msg.Event != model.EventScheduleSubscribe {
		return model.SubscribeRequest{}, errors.New("malformed handshake: expected `schedule:subscribe` event")
	}

	return model.As[model.SubscribeData](msg), nil
}

func isWebsocketRequest(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}

	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}

	return false
}
