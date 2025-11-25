package model

import (
	"encoding/json"
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

const (
	ListModeSequential   ListModeType = "sequential"
	ListModeNextPerRoute ListModeType = "nextPerRoute"
)

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

type ScheduleData struct {
	Trips []Trip `json:"trips"`
}

type SubscribeRequest = Event[SubscribeData]
type ScheduleChange = Event[ScheduleData]
type Heartbeat = Event[struct{}]

func As[O interface{}, D interface{}](event Event[O]) Event[D] {
	var v = any(event.Data).(*D)
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
		ret = struct{}{}
	}

	err = json.Unmarshal(id.Data, ret)
	return Event[interface{}]{
		Event: id.Event,
		Data:  ret,
	}, err
}
