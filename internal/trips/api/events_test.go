package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"map-service/internal/geo"
	"map-service/internal/httpkit"
	"map-service/internal/trips/store"
	"map-service/internal/trips/tracker"
)

// sseEvent is one block of an event stream.
type sseEvent struct {
	ID      int64
	Type    string
	Data    string
	Comment string
	Retry   string
}

// stream is an open event stream read by a goroutine.
type stream struct {
	resp   *http.Response
	events chan sseEvent // closed at the end of the stream
}

// listen runs the database listener that wakes the streams and waits until
// it listens.
func (hs *harness) listen() {
	hs.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hs.t.Cleanup(cancel)
	ready := make(chan struct{})
	var once bool
	go func() {
		_ = hs.store.Listen(ctx, func(n store.Notification) {
			if n == (store.Notification{}) && !once {
				once = true
				close(ready)
			}
			hs.srv.hub.notify(n)
		})
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		hs.t.Fatal("listener did not start")
	}
}

// serve starts a real HTTP server (streams need one).
func (hs *harness) serve() *httptest.Server {
	ts := httptest.NewServer(hs.h)
	hs.t.Cleanup(ts.Close)
	return ts
}

func openStream(t *testing.T, ts *httptest.Server, trip CreateTripResponse, tok string, headers ...string) *stream {
	t.Helper()
	return openStreamURL(t, ts.URL+"/v1/trips/"+trip.Trip.ID+"/events", tok, headers...)
}

func openStreamURL(t *testing.T, url, tok string, headers ...string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	s := &stream{resp: resp, events: make(chan sseEvent, 64)}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev sseEvent
		empty := true
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if !empty {
					s.events <- ev
				}
				ev, empty = sseEvent{}, true
				continue
			}
			empty = false
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "":
				ev.Comment = value
			case "id":
				ev.ID, _ = strconv.ParseInt(value, 10, 64)
			case "event":
				ev.Type = value
			case "data":
				ev.Data = value
			case "retry":
				ev.Retry = value
			}
		}
	}()
	return s
}

// next returns the next event that is not a comment (unless comments is
// set), failing after a while.
func (s *stream) next(t *testing.T, comments bool) sseEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatal("stream ended")
			}
			if ev.Retry != "" || (ev.Comment != "" && !comments) {
				continue
			}
			return ev
		case <-deadline:
			t.Fatal("no event within 5 s")
		}
	}
}

// wantEnd fails unless the stream ends soon (comments are skipped).
func (s *stream) wantEnd(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				return
			}
			if ev.Type != "" {
				t.Fatalf("unexpected event %+v", ev)
			}
		case <-deadline:
			t.Fatal("stream did not end")
		}
	}
}

func TestEventStream(t *testing.T) {
	hs := newHarness(t, nil)
	hs.srv.heartbeat = time.Minute // events must come from notifications
	hs.listen()
	ts := hs.serve()
	trip := hs.createTrip()
	line := lineOf(t, trip.Route)

	s := openStream(t, ts, trip, trip.WatchToken)
	if ct := s.resp.Header.Get("Content-Type"); s.resp.StatusCode != 200 || !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("%d %s", s.resp.StatusCode, ct)
	}
	snap := s.next(t, false)
	var sr SnapshotResponse
	if snap.Type != eventSnapshot || snap.ID != 0 || json.Unmarshal([]byte(snap.Data), &sr) != nil || sr.Trip.ID != trip.Trip.ID || sr.Route.Version != 1 {
		t.Fatalf("first event %+v", snap)
	}

	d := &driverSim{hs: hs, trip: trip, known: 1}
	d.drive(d.along(line, 0, 300, 10)[:4])
	ev := s.next(t, false)
	var pe PositionEvent
	if ev.Type != EventPosition || ev.ID <= 0 || json.Unmarshal([]byte(ev.Data), &pe) != nil || pe.Position.Seq != 4 || len(pe.Points) != 4 {
		t.Fatalf("position event %+v", ev)
	}

	// Off the route: a route event follows the position events.
	d.drive(d.along(line, 40, 500, 10))
	turn := line.PointAt(500)
	var off []PositionIn
	for i := 1; i <= 14; i++ {
		off = append(off, d.fix(geo.Destination(turn, 180, float64(10*i)), 10, 180))
	}
	d.drive(off)
	prev := ev.ID
	var route RouteDTO
	for route.Version == 0 {
		ev = s.next(t, false)
		if ev.ID <= prev {
			t.Fatalf("event ids must increase: %d after %d", ev.ID, prev)
		}
		prev = ev.ID
		if ev.Type == EventRoute {
			if err := json.Unmarshal([]byte(ev.Data), &route); err != nil {
				t.Fatal(err)
			}
		}
	}
	if route.Version != 2 || route.Reason != tracker.ReasonOffRoute || route.Geometry == "" {
		t.Errorf("route event %+v", route)
	}

	// The driver ends the trip: a status event, then the stream closes.
	hs.do("POST", "/v1/trips/"+trip.Trip.ID+"/finish", nil, bearer(trip.DriverToken)...)
	for ev.Type != store.EventStatus {
		ev = s.next(t, false)
	}
	if ev.Data != `{"status": "finished"}` {
		t.Errorf("status event %+v", ev)
	}
	s.wantEnd(t)
	if n := hs.srv.hub.streams(); n != 0 {
		t.Errorf("%d streams still registered", n)
	}
}

func TestEventStreamResume(t *testing.T) {
	hs := newHarness(t, nil)
	ts := hs.serve()
	trip := hs.createTrip()
	line := lineOf(t, trip.Route)
	d := &driverSim{hs: hs, trip: trip, known: 1}
	d.drive(d.along(line, 0, 110, 10)) // 12 fixes: 3 batches, 3 events
	evs, _ := hs.store.EventsAfter(context.Background(), trip.Trip.ID, 0, 10)
	if len(evs) != 3 {
		t.Fatalf("%d events", len(evs))
	}

	// EventSource reconnecting after the first event gets the other two, in
	// order, and no snapshot.
	s := openStream(t, ts, trip, trip.WatchToken, "Last-Event-ID", strconv.FormatInt(evs[0].ID, 10))
	if c := s.next(t, true); c.Comment != "resumed" {
		t.Fatalf("first block %+v", c)
	}
	for _, want := range evs[1:] {
		if got := s.next(t, false); got.ID != want.ID || got.Type != EventPosition {
			t.Errorf("got %+v, want event %d", got, want.ID)
		}
	}
	// The same through the query parameter.
	q := openStreamURL(t, ts.URL+"/v1/trips/"+trip.Trip.ID+"/events?last_event_id="+strconv.FormatInt(evs[1].ID, 10), trip.WatchToken)
	if got := q.next(t, false); got.ID != evs[2].ID {
		t.Errorf("?last_event_id: got %+v", got)
	}

	// Too far behind: a snapshot instead of the backlog.
	hs.srv.maxReplay = 1
	far := openStream(t, ts, trip, trip.WatchToken, "Last-Event-ID", "0")
	if got := far.next(t, false); got.Type != eventSnapshot || got.ID != evs[2].ID {
		t.Errorf("far behind: %+v", got)
	}
}

func TestEventStreamOfAnEndedTrip(t *testing.T) {
	hs := newHarness(t, nil)
	ts := hs.serve()
	trip := hs.createTrip()
	hs.do("POST", "/v1/trips/"+trip.Trip.ID+"/cancel", nil, bearer(trip.DriverToken)...)

	// A new watcher gets the snapshot, then the stream ends...
	s := openStream(t, ts, trip, trip.WatchToken)
	snap := s.next(t, false)
	if snap.Type != eventSnapshot || !strings.Contains(snap.Data, `"status":"cancelled"`) {
		t.Fatalf("snapshot %+v", snap)
	}
	s.wantEnd(t)
	// ...and reconnecting from it answers 204: EventSource gives up.
	again := openStream(t, ts, trip, trip.WatchToken, "Last-Event-ID", strconv.FormatInt(snap.ID, 10))
	if again.resp.StatusCode != http.StatusNoContent {
		t.Errorf("reconnect after the end: %d", again.resp.StatusCode)
	}
	// One that missed the status event still gets it first.
	missed := openStream(t, ts, trip, trip.WatchToken, "Last-Event-ID", "0")
	if ev := missed.next(t, false); ev.Type != store.EventStatus {
		t.Errorf("missed status: %+v", ev)
	}
	missed.wantEnd(t)
}

func TestEventStreamHeartbeatAndLimits(t *testing.T) {
	hs := newHarness(t, nil)
	hs.srv.heartbeat = 50 * time.Millisecond
	ts := hs.serve()
	trip := hs.createTrip()

	var open []*stream
	for range maxStreamsPerTrip {
		open = append(open, openStream(t, ts, trip, trip.WatchToken))
	}
	if c := open[0].next(t, true); c.Type != eventSnapshot {
		t.Fatalf("first block %+v", c)
	}
	if c := open[0].next(t, true); c.Comment != "ping" {
		t.Errorf("want a heartbeat, got %+v", c)
	}
	extra := openStream(t, ts, trip, trip.DriverToken)
	if extra.resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("stream over the limit: %d", extra.resp.StatusCode)
	}

	rec := hs.do("GET", "/v1/trips/"+trip.Trip.ID+"/events", nil, append(bearer(trip.WatchToken), "Last-Event-ID", "x")...)
	wantError(t, rec, http.StatusBadRequest, httpkit.CodeInvalidArgument)

	// Shutting down ends every stream.
	for _, s := range open[1:] {
		if ev := s.next(t, false); ev.Type != eventSnapshot {
			t.Fatalf("first event %+v", ev)
		}
	}
	hs.srv.CloseStreams()
	for _, s := range open {
		s.wantEnd(t)
	}
	deadline := time.Now().Add(2 * time.Second)
	for hs.srv.hub.streams() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := hs.srv.hub.streams(); n != 0 {
		t.Errorf("%d streams left after CloseStreams", n)
	}
}

func TestTrack(t *testing.T) {
	hs := newHarness(t, nil)
	trip := hs.createTrip()
	line := lineOf(t, trip.Route)
	d := &driverSim{hs: hs, trip: trip, known: 1}
	ps := d.along(line, 0, 240, 10) // 25 fixes
	ps[3].TS = ps[3].TS.Add(2 * time.Hour)
	d.drive(ps)

	path := "/v1/trips/" + trip.Trip.ID + "/track"
	var all []TrackPosition
	after, pages := int64(0), 0
	for {
		rec := hs.do("GET", path+"?limit=10&after_seq="+strconv.FormatInt(after, 10), nil, bearer(trip.WatchToken)...)
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		tr := decode[TrackResponse](t, rec)
		pages++
		if len(tr.Routes) != 1 || tr.Routes[0].Version != 1 || tr.Trip.ID != trip.Trip.ID {
			t.Errorf("page %d: %+v", pages, tr)
		}
		all = append(all, tr.Positions...)
		if tr.NextAfterSeq == nil {
			break
		}
		after = *tr.NextAfterSeq
	}
	if pages != 3 || len(all) != 25 {
		t.Fatalf("%d positions in %d pages", len(all), pages)
	}
	for i, p := range all {
		if p.Seq != int64(i+1) || p.RouteVersion != 1 {
			t.Errorf("position %d: %+v", i, p)
		}
	}
	if all[3].Reject != rejectBadTime || all[3].Class != "" || all[4].Class != string(tracker.ClassOn) || all[4].Speed == nil {
		t.Errorf("verdicts %+v / %+v", all[3], all[4])
	}
	for _, q := range []string{"?limit=0", "?limit=99999", "?after_seq=-1", "?limit=x"} {
		wantError(t, hs.do("GET", path+q, nil, bearer(trip.DriverToken)...), 400, httpkit.CodeInvalidArgument)
	}
}

func TestHub(t *testing.T) {
	h := newHub(2)
	a1, ua1, ok1 := h.subscribe("a")
	a2, _, ok2 := h.subscribe("a")
	_, _, ok3 := h.subscribe("a")
	b, ub, okb := h.subscribe("b")
	if !ok1 || !ok2 || ok3 || !okb {
		t.Fatalf("subscribe: %v %v %v %v", ok1, ok2, ok3, okb)
	}
	woken := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	h.notify(store.Notification{TripID: "a", EventID: 1})
	h.notify(store.Notification{TripID: "a", EventID: 2}) // coalesced
	if !woken(a1) || !woken(a2) || woken(a1) || woken(b) {
		t.Error("a trip's notification must wake its streams once, and only them")
	}
	h.notify(store.Notification{})
	if !woken(a1) || !woken(b) {
		t.Error("the zero notification must wake every stream")
	}
	ua1()
	if _, _, ok := h.subscribe("a"); !ok {
		t.Error("a freed slot must be reusable")
	}
	ub()
	if h.streams() != 2 {
		t.Errorf("%d streams", h.streams())
	}
}
