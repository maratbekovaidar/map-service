package api

import (
	"context"
	"sync"

	"map-service/internal/trips/store"
)

// hub wakes the event streams of a trip when the database says it has new
// events. A wake-up carries no data: the stream reads what follows the last
// event it sent, so wake-ups may be coalesced or spurious, never harmful.
type hub struct {
	maxPerTrip int

	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{} // trip id -> wake channels
}

func newHub(maxPerTrip int) *hub {
	return &hub{maxPerTrip: maxPerTrip, subs: map[string]map[chan struct{}]struct{}{}}
}

// subscribe registers a stream of trip. It fails when the trip already has
// maxPerTrip streams (a token holder must not tie up the server).
func (h *hub) subscribe(trip string) (wake <-chan struct{}, unsubscribe func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.subs[trip]
	if len(set) >= h.maxPerTrip {
		return nil, nil, false
	}
	if set == nil {
		set = map[chan struct{}]struct{}{}
		h.subs[trip] = set
	}
	ch := make(chan struct{}, 1)
	set[ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(set, ch)
		if len(set) == 0 {
			delete(h.subs, trip)
		}
	}, true
}

// notify wakes the streams of n's trip; the zero Notification (the listener
// may have missed some) wakes every stream.
func (h *hub) notify(n store.Notification) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n == (store.Notification{}) {
		for _, set := range h.subs {
			wakeAll(set)
		}
		return
	}
	wakeAll(h.subs[n.TripID])
}

func wakeAll(set map[chan struct{}]struct{}) {
	for ch := range set {
		select {
		case ch <- struct{}{}:
		default: // already pending
		}
	}
}

func (h *hub) streams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, set := range h.subs {
		n += len(set)
	}
	return n
}

// RunListener feeds the database's event notifications to the open event
// streams until ctx is done.
func (s *Server) RunListener(ctx context.Context) {
	_ = s.store.Listen(ctx, s.hub.notify)
}
