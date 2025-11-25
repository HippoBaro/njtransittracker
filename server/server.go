package server

import (
	"context"
	"errors"
	"fmt"
	log "log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"njtransittracker/model"

	"golang.org/x/net/websocket"
)

const (
	initialMessageTimeout  = 30 * time.Second
	heartbeatInterval      = 10 * time.Second
	websocketWriteDeadline = 5 * time.Second
	serverShutdownTimeout  = 5 * time.Second
)

type WebsocketServer struct {
	addr     string
	upstream string
	proxy    *httputil.ReverseProxy
}

func NewWebsocketServer(addr, proxyTarget string) (*WebsocketServer, error) {
	upstream, err := url.Parse(proxyTarget)
	if err != nil {
		return nil, fmt.Errorf("parse proxy target %q: %w", proxyTarget, err)
	}

	return &WebsocketServer{
		addr:     addr,
		upstream: proxyTarget,
		proxy:    httputil.NewSingleHostReverseProxy(upstream),
	}, nil
}

func (s *WebsocketServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	wsHandler := websocket.Handler(func(conn *websocket.Conn) {
		s.handleWebsocketConnection(ctx, conn)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isWebsocketRequest(r) {
			wsHandler.ServeHTTP(w, r)
			return
		}

		log.Info("Proxying HTTP request to upstream", "endpoint", s.upstream, "request", r.RequestURI)
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
	log.Info("Initial message received", "remote", conn.Request().RemoteAddr, "message", message)

	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeatTicker.C:
			if err = sendHeartbeat(conn); err != nil {
				log.Warn("Heartbeat failed, closing connection", "err", err, "remote", conn.Request().RemoteAddr)
				return
			}
		}
	}
}

func waitForInitialMessage(conn *websocket.Conn) (model.Event[any], error) {
	if err := conn.SetReadDeadline(time.Now().Add(initialMessageTimeout)); err != nil {
		return model.Event[any]{}, err
	}
	defer conn.SetReadDeadline(time.Time{})

	var raw []byte
	if err := websocket.Message.Receive(conn, &raw); err != nil {
		return model.Event[any]{}, fmt.Errorf("remote failed to send message within intial window: %w", err)
	}

	return model.FromRaw(raw)
}

func sendHeartbeat(conn *websocket.Conn) error {
	if err := conn.SetWriteDeadline(time.Now().Add(websocketWriteDeadline)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})

	return websocket.Message.Send(conn, model.Heartbeat{})
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
