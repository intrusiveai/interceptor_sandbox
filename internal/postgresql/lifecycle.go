package postgresql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var errLocalClose = errors.New("PostgreSQL socket closed by proxy")
var errClientDisconnect = errors.New("PostgreSQL client disconnected")

// ownedSocket tags only net.ErrClosed returned by a socket we deliberately
// closed. Peer EOF, resets, deadlines and protocol errors remain substantive,
// even if a concurrent shutdown has already begun.
type ownedSocket struct {
	net.Conn
	closed atomic.Bool
}

func (c *ownedSocket) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}
func (c *ownedSocket) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	return n, c.classify(err)
}
func (c *ownedSocket) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	return n, c.classify(err)
}
func (c *ownedSocket) classify(err error) error {
	if c.closed.Load() && errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("%w: %w", errLocalClose, err)
	}
	return err
}

type connectionLifecycle struct {
	ctx           context.Context
	cancel        context.CancelFunc
	client        *ownedSocket
	mu            sync.Mutex
	upstream      *ownedSocket
	closing       bool
	reason        string
	failure       error
	failureOrigin string
}

func newLifecycle(raw net.Conn) *connectionLifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	return &connectionLifecycle{ctx: ctx, cancel: cancel, client: &ownedSocket{Conn: raw}}
}
func (l *connectionLifecycle) attach(up net.Conn) (*ownedSocket, error) {
	socket := &ownedSocket{Conn: up}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		socket.Close()
		// stop marks closing before it cancels the context outside the lock.
		return nil, errLocalClose
	}
	l.upstream = socket
	return socket, nil
}
func (l *connectionLifecycle) complete(origin string, err error) {
	reason := origin + "_failure"
	switch {
	case errors.Is(err, errLocalClose):
		err = nil
	case err == context.Canceled && l.ctx.Err() != nil:
		// This context belongs only to this lifecycle; a substantive cause is saved
		// before cancellation, and unrelated errors after cancellation are retained.
		// Only a bare ctx.Err() is expected: joined causes must not be discarded.
		err = nil
	case origin == "client" && (err == nil || errors.Is(err, errClientDisconnect)):
		reason, err = "client_disconnect", nil
	case origin == "upstream" && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)):
		err = fmt.Errorf("PostgreSQL upstream disconnected: %w", err)
	}
	l.stop(reason, origin, err, false)
}
func (l *connectionLifecycle) stop(reason, origin string, err error, timeout bool) {
	l.mu.Lock()
	// A timer that lost to an already initiated teardown is not a query timeout.
	if timeout && l.closing {
		l.mu.Unlock()
		return
	}
	if err != nil && l.failure == nil {
		l.failure = err
		l.failureOrigin = origin
	}
	first := !l.closing
	if first {
		l.closing = true
		l.reason = reason
	}
	up := l.upstream
	l.mu.Unlock()
	if first {
		l.cancel()
		l.client.Close()
		if up != nil {
			up.Close()
		}
	}
}
func (l *connectionLifecycle) outcome() (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reason, l.failureOrigin, l.failure
}

// Stop and expiry synchronize so a finished query cannot acquire a late timeout.
func (l *connectionLifecycle) queryTimer(duration time.Duration) func() {
	var mu sync.Mutex
	finished := false
	timer := time.AfterFunc(duration, func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			l.stop("query_timeout", "query", fmt.Errorf("PostgreSQL query exceeded query_timeout_ms: %w", context.DeadlineExceeded), true)
		}
	})
	return func() { mu.Lock(); finished = true; timer.Stop(); mu.Unlock() }
}
