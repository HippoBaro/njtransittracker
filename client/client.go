package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	log "log/slog"
	"net"
	"time"

	"njtransittracker/model"

	"golang.org/x/net/websocket"
)

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
	writeTimeout   = 5 * time.Second
	dialTimeout    = 10 * time.Second
	messageBufSize = 32
)

var HomeScheduleSubscribe = model.SubscribeRequest{
	Event: model.EventScheduleSubscribe,
	Data: model.SubscribeData{
		RouteStopPairs: "njtbus:46,njtbus:17659;njtbus:48,njtbus:17659;njtbus:49,njtbus:17659;njtrail:4,njtrail:9878",
		Limit:          3,
	},
}

// ScheduleClient maintains a resilient websocket connection that streams schedule updates.
type ScheduleClient struct {
	domain    string
	payload   model.SubscribeRequest
	messageCh chan model.ScheduleChange

	ctx     context.Context
	started bool
}

func NewScheduleClient(ctx context.Context, domain string, payload model.SubscribeRequest) *ScheduleClient {
	return &ScheduleClient{
		domain:    domain,
		payload:   payload,
		messageCh: make(chan model.ScheduleChange, messageBufSize),
		ctx:       ctx,
	}
}

func (c *ScheduleClient) Start() error {
	if c.started {
		return errors.New("schedule client already started")
	}

	c.started = true
	go c.run()

	return nil
}

func (c *ScheduleClient) Schedule() <-chan model.ScheduleChange {
	return c.messageCh
}

func (c *ScheduleClient) run() {
	defer close(c.messageCh)
	backoff := initialBackoff

	for {
		if err := c.connectAndListen(); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Error("connection error", "err", err)
		} else {
			return
		}

		log.Warn("Reconnecting...", "backoff", backoff)
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *ScheduleClient) connectAndListen() error {
	if c.ctx == nil {
		return errors.New("schedule client is not running")
	}

	config, err := websocket.NewConfig(fmt.Sprintf("wss://%s", c.domain), fmt.Sprintf("https://%s", c.domain))
	if err != nil {
		return fmt.Errorf("create websocket config: %w", err)
	}

	config.Dialer = &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}

	dialCtx, cancel := context.WithTimeout(c.ctx, dialTimeout)
	defer cancel()

	conn, err := config.DialContext(dialCtx)
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer conn.Close()

	conn.PayloadType = websocket.TextFrame

	closeCh := make(chan struct{})
	go func() {
		select {
		case <-c.ctx.Done():
			conn.Close()
		case <-closeCh:
		}
	}()
	defer close(closeCh)

	if err = c.sendSubscribe(conn); err != nil {
		return fmt.Errorf("send subscribe payload: %w", err)
	}

	log.Info("Connected. Waiting for schedule updates", "endpoint", config.Location.String())

	for {
		if c.ctx.Err() != nil {
			return c.ctx.Err()
		}

		var message []byte
		if err = websocket.Message.Receive(conn, &message); err != nil {
			if c.ctx.Err() != nil {
				return c.ctx.Err()
			}
			return fmt.Errorf("receive message: %w", err)
		}

		if err = c.deliver(message); err != nil {
			return fmt.Errorf("decoding message: %w", err)
		}
	}
}

func (c *ScheduleClient) sendSubscribe(conn *websocket.Conn) error {
	data, err := json.Marshal(c.payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	if err = conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	defer conn.SetWriteDeadline(time.Time{})

	if _, err = conn.Write(data); err != nil {
		return fmt.Errorf("write subscribe payload: %w", err)
	}

	return nil
}

func (c *ScheduleClient) deliver(message []byte) (err error) {
	event, err := model.FromRaw(message)

	if event.Event == model.EventHeartBeat {
		log.Debug("Received heartbeat from remote.")
	} else if event.Event != model.EventSchedule {
		log.Error("Received unexpected event from remote.", "event", event)
	}

	select {
	case c.messageCh <- model.As[any, model.ScheduleData](event):
	case <-c.ctx.Done():
	}

	return err
}
