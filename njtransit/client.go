package njtransit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	log "log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"njtransittracker/model"
)

const (
	defaultGraphQLURL      = "https://www.njtransit.com/api/graphql/graphql"
	defaultRefreshInterval = 30 * time.Second
	defaultHTTPTimeout     = 10 * time.Second
)

var (
	NYLocation, _ = time.LoadLocation("America/New_York")
)

// Config configures the ETA client.
type Config struct {
	// APIURL is the GraphQL endpoint for NJTransit bus arrivals.
	APIURL string

	// RefreshInterval controls how often the background goroutine refreshes ETAs.
	// If zero, a sane default is used.
	RefreshInterval time.Duration

	// HTTPClient is the client used for HTTP requests. If nil, a default client is used.
	HTTPClient *http.Client
}

// DefaultConfig returns a Config populated with reasonable defaults.
func DefaultConfig() Config {
	return Config{
		APIURL:          defaultGraphQLURL,
		RefreshInterval: defaultRefreshInterval,
		HTTPClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

// ETA represents a single upcoming arrival for a given route at a stop.
type ETA struct {
	StopID      string
	RouteID     string
	Destination string
	VehicleID   *string
	Occupancy   *string
	Departure   time.Time
	Delayed     bool
}

func (e ETA) Equal(other ETA) bool {
	if e.StopID != other.StopID ||
		e.RouteID != other.RouteID ||
		e.Destination != other.Destination ||
		e.Delayed != other.Delayed ||
		!e.Departure.Equal(other.Departure) {
		return false
	}

	// Compare VehicleID
	if (e.VehicleID == nil) != (other.VehicleID == nil) {
		return false
	}
	if e.VehicleID != nil && *e.VehicleID != *other.VehicleID {
		return false
	}

	// Compare Occupancy
	if (e.Occupancy == nil) != (other.Occupancy == nil) {
		return false
	}
	if e.Occupancy != nil && *e.Occupancy != *other.Occupancy {
		return false
	}

	return true
}

func (e ETA) Realtime() bool {
	return e.VehicleID != nil && !e.Delayed
}

type ETAs []ETA

func (e ETAs) Equal(other ETAs) bool {
	return slices.EqualFunc(e, other, func(eta ETA, eta2 ETA) bool {
		return eta.Equal(eta2)
	})
}

func (e ETAs) ToTrips() (ret model.Trips) {
	ret = make([]model.Trip, 0)

	for _, eta := range e {

		// NJTransit defines weird routes with a trailing letter.
		// Eg. 159 and 159R
		// For these, the advertised RouteID is 159 but the Destination fields has actual route prefixed.
		// Eg. 159R NEW YORK VIA RIVER ROAD.
		if strings.HasPrefix(eta.Destination, eta.RouteID) {
			routeID, destination, ok := strings.Cut(eta.Destination, " ")
			if ok {
				eta.RouteID = routeID
				eta.Destination = destination
			}
		}

		// NJTransit gives occupancy information, which we display though color
		// saturation
		color := "8FD4FA" // Defaults to blue
		if eta.Occupancy != nil {
			switch *eta.Occupancy {
			case "EMPTY":
				color = "33C809"
			case "HALF_EMPTY":
				color = "E0F40B"
			case "FULL":
				color = "C80909"
			}
		}

		ret = append(ret, model.Trip{
			RouteName:     eta.RouteID,
			RouteColor:    color,
			Headsign:      eta.Destination,
			ArrivalTime:   int(eta.Departure.Unix()),
			DepartureTime: int(eta.Departure.Unix()),
			IsRealtime:    eta.Realtime(),
		})
	}

	return ret
}

type trackedRoute struct {
	RouteID, StopID string
}

// ETAClient maintains a background refresh loop for ETAs fetched from
// NJTransit's public GraphQL bus arrivals endpoint.
//
// Call Track to add stop IDs to be tracked, Start to begin the background
// refresh loop, FetchNow to trigger an immediate refresh, and GetETA /
// GetNextETAs to query the in-memory ETA cache.
type ETAClient struct {
	cfg             Config
	ctx             context.Context
	httpClient      *http.Client
	refreshInterval time.Duration

	fetchNow chan struct{}

	mu           sync.RWMutex
	started      bool
	notify       []chan error
	lastFetch    time.Time
	trackedStops map[trackedRoute]struct{}
	etas         map[trackedRoute][]ETA
}

// NewETAClient creates a new ETA client. Call Start to begin the refresh loop.
func NewETAClient(ctx context.Context, cfg Config) (*ETAClient, error) {
	if ctx == nil {
		return nil, errors.New("context must not be nil")
	}

	if cfg.APIURL == "" {
		cfg.APIURL = defaultGraphQLURL
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	return &ETAClient{
		cfg:             cfg,
		ctx:             ctx,
		httpClient:      cfg.HTTPClient,
		refreshInterval: cfg.RefreshInterval,
		fetchNow:        make(chan struct{}, 1),
		notify:          make([]chan error, 0),
		trackedStops:    make(map[trackedRoute]struct{}),
		etas:            make(map[trackedRoute][]ETA),
	}, nil
}

// Start launches the background goroutine that periodically refreshes ETAs.
func (c *ETAClient) Start() error {
	if c == nil {
		return errors.New("ETA client is nil")
	}

	c.mu.Lock()
	if c.started {
		return errors.New("ETA client already started")
	}
	c.mu.Unlock()

	c.started = true
	c.fetchNow <- struct{}{}
	go c.run()
	return nil
}

// Track adds a stopID to the list of tracked stops.
func (c *ETAClient) Track(routeID, stopID string) {
	if c == nil || stopID == "" {
		return
	}

	c.mu.Lock()
	if c.trackedStops == nil {
		c.trackedStops = make(map[trackedRoute]struct{})
	}

	tracked := trackedRoute{
		RouteID: routeID,
		StopID:  stopID,
	}
	c.trackedStops[tracked] = struct{}{}
	c.mu.Unlock()
}

func (c *ETAClient) Notify() (<-chan error, func()) {
	ret := make(chan error)
	c.mu.Lock()
	c.notify = append(c.notify, ret)
	c.mu.Unlock()

	return ret, func() {
		c.mu.Lock()
		c.notify = slices.DeleteFunc(c.notify, func(c chan error) bool {
			return c == ret
		})
		c.mu.Unlock()
	}
}

func (c *ETAClient) UpToDate() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.trackedStops) == 0 || time.Since(c.lastFetch) < 2*c.cfg.RefreshInterval
}

// GetNextETAs returns up to limit arrivals for the given route at the tracked stop.
// If limit is zero or negative, all matching ETAs are returned.
func (c *ETAClient) GetNextETAs(routeID, stopID string) model.Trips {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tracked := trackedRoute{
		RouteID: routeID,
		StopID:  stopID,
	}
	arrivals := c.etas[tracked]

	if len(arrivals) == 0 {
		return nil
	}

	// Filter?
	var filtered = slices.Clone(arrivals)
	if len(filtered) == 0 {
		return nil
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Departure.Before(filtered[j].Departure)
	})

	return ETAs(filtered).ToTrips()
}

func (c *ETAClient) run() {
	ticker := time.NewTicker(c.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
		case <-c.fetchNow:
		}

		err := c.refreshAllTrackedRoutes()

		// Take a snapshot of the current notifier channels under read lock so we
		// can iterate without holding the mutex and avoid races with Notify().
		c.mu.RLock()
		notifiers := append([]chan error(nil), c.notify...)
		c.mu.RUnlock()

		// Send notifications in a non-blocking fashion so a single slow or dead
		// listener cannot stall the entire refresh loop.
		for _, ch := range notifiers {
			select {
			case ch <- err:
			default:
			}
		}

		if err == nil {
			c.mu.Lock()
			c.lastFetch = time.Now()
			c.mu.Unlock()
		}
	}
}

func (c *ETAClient) refreshAllTrackedRoutes() error {
	stops := c.snapshotTrackedRoutes()
	if len(stops) == 0 {
		return nil
	}

	// Build the set of unique stops so that we can call getBusArrivalsByStopID
	// once per stop ID.
	stopIDs := make(map[string]struct{})
	for _, s := range stops {
		stopIDs[s.StopID] = struct{}{}
	}

	// Build a single GraphQL query with:
	//   - One getBusArrivalsByStopID per unique stop ID
	//   - One getBusDV5 per (route, stop) pair
	var (
		parts        []string
		now          = time.Now()
		etasByKey    = make(map[trackedRoute][]ETA)
		stopAliases  = make(map[string]string)       // alias -> stopID
		dvAliases    = make(map[string]trackedRoute) // alias -> (route, stop)
		stopAliasIdx int
		dvAliasIdx   int
	)

	// Per-stop arrivals.
	for stopID := range stopIDs {
		fieldName := fmt.Sprintf("s_%d", stopAliasIdx)
		stopAliasIdx++
		stopAliases[fieldName] = stopID

		parts = append(parts, fmt.Sprintf(`
  %s: getBusArrivalsByStopID(stopID: %q) {
    destination
    route
    time
    vehicleID
    capacity
    departingIn
  }`, fieldName, stopID))
	}

	// Per (route, stop) DV5.
	for _, s := range stops {
		fieldName := fmt.Sprintf("d_%d", dvAliasIdx)
		dvAliasIdx++
		dvAliases[fieldName] = s

		parts = append(parts, fmt.Sprintf(`
  %s: getBusDV5(route: %q, stop: %q) {
    header
    passload
    publicRoute
    departuretime
    vehicleId
  }`, fieldName, s.RouteID, s.StopID))
	}

	query := fmt.Sprintf("query getBusDV5AndArrivals {\n%s\n}", strings.Join(parts, "\n"))

	reqBody, err := json.Marshal(graphQLRequest{
		OperationName: "getBusDV5AndArrivals",
		Variables:     nil,
		Query:         query,
	})
	if err != nil {
		return fmt.Errorf("marshal batch request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.cfg.APIURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create batch request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("perform batch request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var gqlResp graphQLBatchResponse
	if err = json.NewDecoder(resp.Body).Decode(&gqlResp); err != nil {
		return fmt.Errorf("decode batch response: %w", err)
	}

	// First, process arrivals by stop ID.
	for alias, stopID := range stopAliases {
		raw, ok := gqlResp.Data[alias]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}

		var arrivals []arrivalByStop
		if err := json.Unmarshal(raw, &arrivals); err != nil {
			log.Warn("Failed to decode arrivals by stop", "stopID", stopID, "err", err)
			continue
		}

		for _, a := range arrivals {
			etaTime, err := parseBusArrivalTime(a.Time, now, NYLocation)
			if err != nil {
				log.Warn("Failed to parse time from getBusArrivalsByStopID", "time", a.Time, "err", err)
				continue
			}

			tracked := trackedRoute{RouteID: a.Route, StopID: stopID}
			eta := ETA{
				StopID:      stopID,
				RouteID:     a.Route,
				Destination: a.Header,
				Departure:   etaTime,
				Delayed:     strings.HasPrefix(a.DepartingIn, "DLY"),
			}

			if len(a.VehicleID) > 0 && a.VehicleID != "EMPTY" && a.VehicleID != "no data" {
				eta.VehicleID = &a.VehicleID
			}
			if len(a.Occupancy) > 0 && a.Occupancy != "no data" {
				eta.Occupancy = &a.Occupancy
			}

			etasByKey[tracked] = append(etasByKey[tracked], eta)
		}
	}

	// Then, fill in gaps from DV5 where we did not get arrivalsByStop data for
	// a given (route, stop) pair.
	for alias, s := range dvAliases {
		raw, ok := gqlResp.Data[alias]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}

		// If we already have arrivals from getBusArrivalsByStopID for this
		// pair, prefer them and skip DV5.
		key := trackedRoute{RouteID: s.RouteID, StopID: s.StopID}
		if len(etasByKey[key]) > 0 {
			continue
		}

		var arrivals []arrivalDV
		if err := json.Unmarshal(raw, &arrivals); err != nil {
			log.Warn("Failed to decode DV5 arrivals", "routeID", s.RouteID, "stopID", s.StopID, "err", err)
			continue
		}

		for _, a := range arrivals {
			etaTime, err := parseBusArrivalTime(a.DepartureTime, now, NYLocation)
			if err != nil {
				log.Warn("Failed to parse time from getBusDV5", "time", a.DepartureTime, "err", err)
				continue
			}

			// If this time has already passed today, assume it's for tomorrow.
			if etaTime.Before(now) {
				etaTime = etaTime.Add(24 * time.Hour)
			}

			eta := ETA{
				StopID:      s.StopID,
				RouteID:     a.Route,
				Destination: a.Header,
				Departure:   etaTime,
			}

			if len(a.VehicleID) > 0 && a.VehicleID != "EMPTY" && a.VehicleID != "no data" {
				eta.VehicleID = &a.VehicleID
			}
			if len(a.Occupancy) > 0 && a.Occupancy != "no data" {
				eta.Occupancy = &a.Occupancy
			}

			etasByKey[key] = append(etasByKey[key], eta)
		}
	}

	// Surface any top-level GraphQL errors if we ended up with no ETAs at all.
	if len(etasByKey) == 0 && len(gqlResp.Errors) > 0 {
		return fmt.Errorf("graphql error: %s", gqlResp.Errors[0].Message)
	}

	// Sort and store per (route, stop).
	c.mu.Lock()
	for key, list := range etasByKey {
		sort.Slice(list, func(i, j int) bool {
			return list[i].Departure.Before(list[j].Departure)
		})
		c.etas[key] = list
	}
	c.mu.Unlock()

	return nil
}

func (c *ETAClient) snapshotTrackedRoutes() []trackedRoute {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stops := make([]trackedRoute, 0, len(c.trackedStops))
	for id := range c.trackedStops {
		stops = append(stops, id)
	}
	return stops
}

type variables struct {
	Stop   string `json:"stop"`
	StopID string `json:"stopID"`
	Route  string `json:"route"`
}

type graphQLRequest struct {
	OperationName string      `json:"operationName"`
	Variables     interface{} `json:"variables"`
	Query         string      `json:"query"`
}

type graphQLBatchResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type arrivalDV struct {
	Header        string `json:"header"`
	Occupancy     string `json:"passload"`
	Route         string `json:"publicRoute"`
	DepartureTime string `json:"departuretime"`
	VehicleID     string `json:"vehicleID"`
}

type arrivalByStop struct {
	Header      string `json:"destination"`
	Route       string `json:"route"`
	Time        string `json:"time"`
	VehicleID   string `json:"vehicleID"`
	Occupancy   string `json:"capacity"`
	DepartingIn string `json:"departingIn"`
}

func parseBusArrivalTime(value string, now time.Time, loc *time.Location) (time.Time, error) {
	const layout = "3:04 PM"

	parsed, err := time.ParseInLocation(layout, strings.TrimSpace(value), loc)
	if err != nil {
		return time.Time{}, err
	}

	now = now.In(loc)
	departure := time.Date(
		now.Year(),
		now.Month(),
		now.Day(),
		parsed.Hour(),
		parsed.Minute(),
		0, 0, loc,
	)

	// If this time has already passed today, assume it's for tomorrow.
	if departure.Before(now) {
		departure = departure.Add(24 * time.Hour)
	}

	return departure, nil
}
