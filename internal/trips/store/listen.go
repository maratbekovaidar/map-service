package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Notification says that trip TripID has new events, up to EventID. The zero
// Notification means "notifications may have been missed": Listen sends it
// whenever it starts listening, the first time and after every reconnect,
// and a receiver should then re-read everything it follows.
type Notification struct {
	TripID  string
	EventID int64
}

// Listen delivers event notifications to fn until ctx is done, on a
// connection of its own; it reconnects when that connection breaks. fn runs
// on Listen's goroutine and should not block. Listen returns ctx.Err().
func (s *Store) Listen(ctx context.Context, fn func(Notification)) error {
	backoff := 100 * time.Millisecond
	for {
		listened := s.listenOnce(ctx, fn)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if listened {
			backoff = 100 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 5*time.Second)
	}
}

// listenOnce listens until the connection fails; it reports whether it got
// as far as listening.
func (s *Store) listenOnce(ctx context.Context, fn func(Notification)) bool {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return false
	}
	fn(Notification{})
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true
		}
		if note, ok := parseNotification(n.Payload); ok {
			fn(note)
		}
	}
}

func parseNotification(payload string) (Notification, bool) {
	id, ev, ok := strings.Cut(payload, ":")
	if !ok || !validID(id) {
		return Notification{}, false
	}
	n, err := strconv.ParseInt(ev, 10, 64)
	if err != nil || n <= 0 {
		return Notification{}, false
	}
	return Notification{TripID: id, EventID: n}, true
}
