package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"map-service/internal/httpkit"
	"map-service/internal/trips/store"
)

// GET /v1/trips/{id}/events is a Server-Sent Events stream of the trip.
//
//   - Without Last-Event-ID it starts with a "snapshot" event (the same body
//     as GET /v1/trips/{id}) whose id is the trip's latest event, then sends
//     every new event: "position", "route", "status".
//   - With Last-Event-ID (EventSource sends it when it reconnects) it sends
//     the events after that one instead; if too many were missed, a fresh
//     snapshot replaces them.
//   - A "status" event ends the trip and the stream. Reconnecting to an
//     ended trip with nothing left to send answers 204, which makes
//     EventSource stop.
//
// Quiet streams get a comment line every heartbeat, which keeps proxies and
// mobile NATs from dropping them.
const (
	eventSnapshot = "snapshot"
	// maxStreamsPerTrip bounds the open streams of one trip (both roles).
	maxStreamsPerTrip = 4
	// retryMillis is the reconnection delay suggested to EventSource.
	retryMillis = 3000
	eventsPage  = 500
)

// CloseStreams ends every open event stream (server shutdown): they would
// otherwise hold the graceful shutdown until its deadline.
func (s *Server) CloseStreams() { s.closeOnce.Do(func() { close(s.closing) }) }

func lastEventID(r *http.Request) (int64, bool, error) {
	v := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if v == "" {
		v = strings.TrimSpace(r.URL.Query().Get("last_event_id"))
	}
	if v == "" {
		return 0, false, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 0 {
		return 0, false, httpkit.BadRequest("Last-Event-ID must be an event id from this stream")
	}
	return id, true, nil
}

// sseWriter writes events and flushes them through any wrapping writers.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (sw sseWriter) event(id int64, typ string, data []byte) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "id: %d\nevent: %s\ndata: %s\n\n", id, typ, bytes.TrimRight(data, "\n"))
	if _, err := sw.w.Write(b.Bytes()); err != nil {
		return err
	}
	return sw.rc.Flush()
}

func (sw sseWriter) comment(text string) error {
	if _, err := fmt.Fprintf(sw.w, ": %s\n\n", text); err != nil {
		return err
	}
	return sw.rc.Flush()
}

func ended(e store.Event) bool { return e.Type == store.EventStatus }

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, c caller) error {
	ctx := r.Context()
	last, resume, err := lastEventID(r)
	if err != nil {
		return err
	}
	// Subscribe before reading, so that an event committed in between wakes
	// the stream instead of being missed.
	wake, unsubscribe, ok := s.hub.subscribe(c.trip.ID)
	if !ok {
		return &httpkit.Error{Status: http.StatusTooManyRequests, Code: httpkit.CodeRateLimited,
			Message: fmt.Sprintf("at most %d event streams per trip", maxStreamsPerTrip), RetryAfter: 10 * time.Second}
	}
	defer unsubscribe()

	// What to send first: a snapshot, or the events the client missed.
	var (
		snap    *SnapshotResponse
		pending []store.Event
	)
	if resume {
		pending, err = s.store.EventsAfter(ctx, c.trip.ID, last, s.maxReplay+1)
		if err != nil {
			return httpkit.Internal(err)
		}
		if len(pending) > s.maxReplay {
			pending, resume = nil, false
		}
	}
	if !resume {
		if snap, err = s.snapshot(ctx, c.trip.ID); err != nil {
			return err
		}
		last = snap.LastEventID
	}
	if resume && len(pending) == 0 && c.trip.Status != store.Active {
		w.WriteHeader(http.StatusNoContent) // nothing more will ever come
		return nil
	}

	// Lift the server's read and write timeouts: either would cut the stream
	// (an expired read deadline cancels the request context, too).
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := sseWriter{w: w, rc: rc}
	log := httpkit.LoggerFrom(ctx)

	// From here on errors can only end the stream: the status line is sent.
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", retryMillis); err != nil {
		return nil
	}
	if snap != nil {
		if err := sw.event(snap.LastEventID, eventSnapshot, httpkit.MarshalJSON(snap)); err != nil {
			return nil
		}
		if snap.Trip.Status != string(store.Active) {
			return nil
		}
	} else if err := sw.comment("resumed"); err != nil {
		return nil
	}

	beat := time.NewTicker(s.heartbeat)
	defer beat.Stop()
	for {
		for _, e := range pending {
			if err := sw.event(e.ID, e.Type, e.Data); err != nil {
				return nil
			}
			last = e.ID
			if ended(e) {
				return nil
			}
		}
		if len(pending) < eventsPage { // else there may be more: read on at once
			select {
			case <-ctx.Done():
				return nil
			case <-s.closing:
				return nil
			case <-wake:
			case <-beat.C:
				if err := sw.comment("ping"); err != nil {
					return nil
				}
				// Also look for events, in case a notification was lost.
			}
		}
		pending, err = s.store.EventsAfter(ctx, c.trip.ID, last, eventsPage)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Warn("event stream: reading events failed", "trip", c.trip.ID, "err", err)
			}
			return nil
		}
	}
}
