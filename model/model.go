package model

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

type EventType string

const (
	EventSchedule          EventType = "schedule"
	EventScheduleSubscribe           = "schedule:subscribe"
	EventHeartBeat         EventType = "heartbeat"
)

type Event[T any] struct {
	Event EventType `json:"event"`
	Data  T         `json:"data"`
}

type ListModeType string

type SubscribeData struct {
	FeedCode        *string       `json:"feedCode,omitempty"`
	RouteStopPairs  string        `json:"routeStopPairs"`
	Limit           int           `json:"limit"`
	SortByDeparture bool          `json:"sortByDeparture"`
	ListMode        *ListModeType `json:"listMode,omitempty"`
}

type Trip struct {
	TripID        string `json:"tripId"`
	StopID        string `json:"stopId"`
	RouteID       string `json:"routeId"`
	RouteName     string `json:"routeName"`
	RouteColor    string `json:"routeColor"`
	StopName      string `json:"stopName"`
	Headsign      string `json:"headsign"`
	ArrivalTime   int    `json:"arrivalTime"`
	DepartureTime int    `json:"departureTime"`
	IsRealtime    bool   `json:"isRealtime"`
}

func (t Trip) Equal(other Trip) bool {
	return t.TripID == other.TripID &&
		t.StopID == other.StopID &&
		t.RouteID == other.RouteID &&
		t.RouteName == other.RouteName &&
		t.RouteColor == other.RouteColor &&
		t.StopName == other.StopName &&
		t.Headsign == other.Headsign &&
		t.ArrivalTime == other.ArrivalTime &&
		t.DepartureTime == other.DepartureTime &&
		t.IsRealtime == other.IsRealtime
}

type Trips []Trip

func (s Trips) WithRouteName(name string) Trips {
	for i := range s {
		s[i].RouteName = name
	}

	return s
}

func (s Trips) Sort() Trips {
	slices.SortFunc(s, func(a, b Trip) int {
		return cmp.Compare(a.DepartureTime, b.DepartureTime)
	})

	return s
}

func (s Trips) Trim(limit int) Trips {
	if len(s) > limit {
		s = s[:limit]
	}

	return s
}

func (s Trips) Equal(other Trips) bool {
	// Compare trips using Trip.Equal
	if len(s) != len(other) {
		return false
	}

	for i, trip := range s {
		if !trip.Equal(other[i]) {
			return false
		}
	}

	return true
}

func (s Trips) FilterHeadsign(headsign string) (ret Trips) {
	for _, trip := range s {
		if strings.Contains(trip.Headsign, headsign) {
			ret = append(ret, trip)
		}
	}

	return ret
}

func (s Trips) FilterPriorArrival(arrivalTime time.Time) (ret Trips) {
	for _, trip := range s {
		if trip.ArrivalTime >= int(arrivalTime.Unix()) {
			ret = append(ret, trip)
		}
	}

	return ret
}

type ScheduleData struct {
	Trips []Trip `json:"trips"`
}

type SubscribeRequest = Event[SubscribeData]
type ScheduleChange = Event[ScheduleData]
type Heartbeat = Event[struct{}]

func As[D interface{}](event Event[any]) Event[D] {
	var v = event.Data.(*D)
	return Event[D]{Event: event.Event, Data: *v}
}

func FromRaw(raw []byte) (_ Event[any], err error) {
	var id struct {
		Event EventType       `json:"event"`
		Data  json.RawMessage `json:"data"`
	}

	if err = json.Unmarshal(raw, &id); err != nil {
		return Event[any]{}, err
	}

	var ret interface{}

	switch id.Event {
	case EventScheduleSubscribe:
		ret = &SubscribeData{}
	case EventSchedule:
		ret = &ScheduleData{}
	case EventHeartBeat:
		ret = &struct{}{}
	}

	err = json.Unmarshal(id.Data, ret)
	return Event[interface{}]{
		Event: id.Event,
		Data:  ret,
	}, err
}
