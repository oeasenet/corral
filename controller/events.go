package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Event is one line of the controller's activity log.
type Event struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// EventLog keeps the most recent events in memory (and mirrors them to stdout).
type EventLog struct {
	mu       sync.Mutex
	events   []Event
	capacity int
}

func NewEventLog(capacity int) *EventLog {
	if capacity <= 0 {
		capacity = 200
	}
	return &EventLog{capacity: capacity}
}

func (l *EventLog) add(level, msg string) {
	log.Printf("%s %s", level, msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, Event{Time: time.Now(), Level: level, Message: msg})
	if len(l.events) > l.capacity {
		l.events = l.events[len(l.events)-l.capacity:]
	}
}

func (l *EventLog) Infof(format string, args ...any)  { l.add("INFO", fmt.Sprintf(format, args...)) }
func (l *EventLog) Warnf(format string, args ...any)  { l.add("WARN", fmt.Sprintf(format, args...)) }
func (l *EventLog) Errorf(format string, args ...any) { l.add("ERROR", fmt.Sprintf(format, args...)) }

// List returns the events, newest first.
func (l *EventLog) List() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, 0, len(l.events))
	for i := len(l.events) - 1; i >= 0; i-- {
		out = append(out, l.events[i])
	}
	return out
}
