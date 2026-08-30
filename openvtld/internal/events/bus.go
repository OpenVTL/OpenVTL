// Package events is the in-process pub/sub bus feeding SSE clients and
// the persistent event log. Slow subscribers are dropped, never blocked
// on — a stuck browser must not stall inventory collection.
package events

import (
	"encoding/json"
	"sync"
	"time"
)

type Event struct {
	TS      time.Time      `json:"ts"`
	Kind    string         `json:"kind"`    // e.g. cart_moved, drive_activity, pool_stats
	Subject string         `json:"subject"` // e.g. DDNW01L5, drive:0
	Data    map[string]any `json:"data,omitempty"`
}

func (e Event) JSON() []byte {
	b, _ := json.Marshal(e)
	return b
}

type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewBus() *Bus {
	return &Bus{subs: make(map[chan Event]struct{})}
}

// Subscribe returns a buffered event channel and an unsubscribe func.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

// Publish fans out without blocking; full subscriber buffers lose events
// (SSE clients resync from REST on reconnect).
func (b *Bus) Publish(kind, subject string, data map[string]any) {
	ev := Event{TS: time.Now().UTC(), Kind: kind, Subject: subject, Data: data}
	b.mu.Lock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	b.mu.Unlock()
}
