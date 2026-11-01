package postgresql

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Exercise the production TCP/TLS proxy and its independent forwarding loops.
// Only the private PostgreSQL peer is scripted, to control otherwise rare races.
type lifecycleFixture struct {
	p        *Proxy
	client   *tls.Conn
	frontend *pgproto3.Frontend
	upstream net.Conn
	backend  *pgproto3.Backend
	events   string
}

func startLifecycle(t *testing.T, timeout int, after func(context.Context) error) *lifecycleFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "pg-life-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	rec, err := recording.Open(filepath.Join(root, "events"), "lifecycle-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	blobs, err := blobstore.Open(filepath.Join(root, "blobs"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Database: "support", Roles: []Role{{Name: "agent"}}}
	cfg.Defaults()
	cfg.QueryTimeoutMS = timeout
	serverTLS, clientTLS := testTLS(t)
	e := &Engine{ID: "database", Config: cfg, Root: root, socketDir: root, Recorder: rec, Blobs: blobs}
	listener, err := net.Listen("unix", filepath.Join(root, ".s.PGSQL.5432"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	peer := make(chan net.Conn, 1)
	peerErr := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			peerErr <- err
			return
		}
		c.SetDeadline(time.Now().Add(10 * time.Second))
		b := pgproto3.NewBackend(c, c)
		if _, err = b.ReceiveStartupMessage(); err == nil {
			err = send(c, &pgproto3.AuthenticationOk{})
		}
		if err == nil {
			err = send(c, &pgproto3.ReadyForQuery{TxStatus: 'I'})
		}
		if err != nil {
			c.Close()
			peerErr <- err
			return
		}
		peer <- c
	}()
	p := &Proxy{Engine: e, TLS: serverTLS, After: after}
	if err = p.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	raw, err := net.DialTimeout("tcp", p.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	if err = send(raw, &pgproto3.SSLRequest{}); err != nil {
		t.Fatal(err)
	}
	var ack [1]byte
	if _, err = io.ReadFull(raw, ack[:]); err != nil || ack[0] != 'S' {
		t.Fatalf("SSL negotiation: %q %v", ack, err)
	}
	client := tls.Client(raw, clientTLS)
	if err = client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err = send(client, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber, Parameters: map[string]string{"user": "agent", "database": "support"}}); err != nil {
		t.Fatal(err)
	}
	f := pgproto3.NewFrontend(client, client)
	for {
		m, err := f.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	var upstream net.Conn
	select {
	case upstream = <-peer:
	case err := <-peerErr:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("backend startup timed out")
	}
	t.Cleanup(func() { upstream.Close() })
	return &lifecycleFixture{p: p, client: client, frontend: f, upstream: upstream, backend: pgproto3.NewBackend(upstream, upstream), events: filepath.Join(root, "events")}
}
func (x *lifecycleFixture) query(t *testing.T, sql string) {
	t.Helper()
	if err := send(x.client, &pgproto3.Query{String: sql}); err != nil {
		t.Fatal(err)
	}
	m, err := x.backend.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if q, ok := m.(*pgproto3.Query); !ok || q.String != sql {
		t.Fatalf("backend query: %v", m)
	}
}
func (x *lifecycleFixture) reply(t *testing.T, tag string, status byte) {
	t.Helper()
	for _, m := range []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte(tag)}, &pgproto3.ReadyForQuery{TxStatus: status}} {
		if err := send(x.upstream, m); err != nil {
			t.Fatal(err)
		}
	}
}
func (x *lifecycleFixture) ready(t *testing.T) {
	t.Helper()
	for {
		m, err := x.frontend.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := m.(*pgproto3.ReadyForQuery); ok {
			return
		}
	}
}
func (x *lifecycleFixture) awaitClosed(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		x.p.mu.Lock()
		active := len(x.p.clients)
		x.p.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("proxy did not finish connection teardown")
}
func (x *lifecycleFixture) assertEvidence(t *testing.T, reason, failure, outcome, status string) {
	t.Helper()
	x.awaitClosed(t)
	events, err := recording.VerifyFile(x.events)
	if err != nil {
		t.Fatal(err)
	}
	closed, failed := 0, 0
	for _, event := range events {
		var semantic struct {
			Detail map[string]any `json:"detail"`
		}
		if err = json.Unmarshal(event.Semantic, &semantic); err != nil {
			t.Fatal(err)
		}
		d := semantic.Detail
		switch event.Kind {
		case "database_connection_closed":
			closed++
			if d["close_reason"] != reason || d["expected_close"] != (failure == "") || d["transaction_outcome"] != outcome || d["rollback_confirmed"] != false || d["last_observed_transaction_status"] != status {
				t.Errorf("closed evidence: %s", event.Semantic)
			}
		case "database_connection_failed":
			failed++
			if failure == "" || !strings.Contains(d["reason"].(string), failure) || d["transaction_outcome"] != outcome {
				t.Errorf("failure evidence: %s", event.Semantic)
			}
		}
	}
	wantFailed := 0
	if failure != "" {
		wantFailed = 1
	}
	if closed != 1 || failed != wantFailed {
		t.Fatalf("closed=%d failed=%d; want 1,%d", closed, failed, wantFailed)
	}
}
func TestProxyConnectionLifecycle(t *testing.T) {
	t.Run("idle shutdown after observed commit", func(t *testing.T) {
		x := startLifecycle(t, 5000, nil)
		x.query(t, "BEGIN")
		x.reply(t, "BEGIN", 'T')
		x.ready(t)
		x.query(t, "COMMIT")
		x.reply(t, "COMMIT", 'I')
		x.ready(t)
		x.p.Close()
		x.assertEvidence(t, "proxy_shutdown", "", "no_pending_transaction", "I")
		events, _ := recording.VerifyFile(x.events)
		committed := false
		for _, e := range events {
			if e.Kind == "database_statement_outcome" && strings.Contains(string(e.Semantic), "COMMIT") {
				committed = true
			}
		}
		if !committed {
			t.Fatal("observed commit missing")
		}
	})
	for _, terminate := range []bool{true, false} {
		name := "client EOF"
		if terminate {
			name = "client Terminate"
		}
		t.Run(name, func(t *testing.T) {
			x := startLifecycle(t, 5000, nil)
			if terminate {
				if err := send(x.client, &pgproto3.Terminate{}); err != nil {
					t.Fatal(err)
				}
			} else {
				x.client.Close()
			}
			if terminate {
				x.assertEvidence(t, "client_disconnect", "", "no_pending_transaction", "I")
			} else {
				// Closing without a protocol Terminate is an incomplete exchange in pgproto3.
				x.assertEvidence(t, "client_failure", "unexpected EOF", "no_pending_transaction", "I")
			}
		})
	}
	t.Run("unexpected upstream loss", func(t *testing.T) {
		x := startLifecycle(t, 5000, nil)
		x.query(t, "UPDATE accounts SET amount=42")
		x.upstream.Close()
		x.assertEvidence(t, "upstream_failure", "PostgreSQL upstream disconnected: unexpected EOF", "unconfirmed", "I")
	})
	t.Run("query timeout", func(t *testing.T) {
		x := startLifecycle(t, 200, nil)
		x.query(t, "SELECT pg_sleep(20)")
		x.assertEvidence(t, "query_timeout", "query_timeout_ms: context deadline exceeded", "unconfirmed", "I")
	})
	t.Run("open transaction shutdown", func(t *testing.T) {
		x := startLifecycle(t, 5000, nil)
		x.query(t, "BEGIN")
		x.reply(t, "BEGIN", 'T')
		x.ready(t)
		x.p.Close()
		x.assertEvidence(t, "proxy_shutdown", "", "unconfirmed", "T")
	})
	t.Run("pending exchange shutdown", func(t *testing.T) {
		x := startLifecycle(t, 5000, nil)
		x.query(t, "COMMIT")
		x.p.Close()
		x.assertEvidence(t, "proxy_shutdown", "", "unconfirmed", "I")
	})
	t.Run("protocol failure survives secondary closes", func(t *testing.T) {
		x := startLifecycle(t, 5000, nil)
		x.query(t, "SELECT 1")
		if err := send(x.upstream, &pgproto3.CopyOutResponse{}); err != nil {
			t.Fatal(err)
		}
		x.assertEvidence(t, "upstream_failure", "unsupported PostgreSQL backend reply *pgproto3.CopyOutResponse", "unconfirmed", "I")
	})
	t.Run("idle upstream loss is still a failure", func(t *testing.T) {
		x := startLifecycle(t, 5000, nil)
		x.upstream.Close()
		x.assertEvidence(t, "upstream_failure", "PostgreSQL upstream disconnected", "no_pending_transaction", "I")
	})
	for _, pending := range []bool{false, true} {
		name := "idle fatal database error"
		if pending {
			name = "pending fatal database error"
		}
		t.Run(name, func(t *testing.T) {
			x := startLifecycle(t, 5000, nil)
			outcome := "no_pending_transaction"
			if pending {
				x.query(t, "UPDATE accounts SET amount=42")
				outcome = "unconfirmed"
			}
			if err := send(x.upstream, &pgproto3.ErrorResponse{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}); err != nil {
				t.Fatal(err)
			}
			x.upstream.Close()
			x.assertEvidence(t, "upstream_failure", "PostgreSQL FATAL (57P01): terminating connection due to administrator command", outcome, "I")
			events, _ := recording.VerifyFile(x.events)
			observed := false
			for _, e := range events {
				if e.Kind == "database_error" && strings.Contains(string(e.Semantic), "57P01") {
					observed = true
				}
			}
			if !observed {
				t.Fatal("original fatal database diagnostic missing")
			}
		})
	}
	t.Run("transaction idle read timeout is a failure", func(t *testing.T) {
		x := startLifecycle(t, 200, nil)
		x.query(t, "BEGIN")
		x.reply(t, "BEGIN", 'T')
		x.ready(t)
		x.assertEvidence(t, "client_failure", "i/o timeout", "unconfirmed", "T")
	})
	t.Run("database error and aborted transaction survive shutdown", func(t *testing.T) {
		x := startLifecycle(t, 5000, nil)
		x.query(t, "BEGIN")
		x.reply(t, "BEGIN", 'T')
		x.ready(t)
		x.query(t, "SELECT 1/0")
		if err := send(x.upstream, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "22012", Message: "division by zero"}); err != nil {
			t.Fatal(err)
		}
		if err := send(x.upstream, &pgproto3.ReadyForQuery{TxStatus: 'E'}); err != nil {
			t.Fatal(err)
		}
		x.ready(t)
		x.p.Close()
		x.assertEvidence(t, "proxy_shutdown", "", "unconfirmed", "E")
		events, _ := recording.VerifyFile(x.events)
		observed := false
		for _, e := range events {
			if e.Kind == "database_error" && strings.Contains(string(e.Semantic), "22012") {
				observed = true
			}
		}
		if !observed {
			t.Fatal("original database error missing")
		}
	})
	t.Run("substantive failure concurrent with cancellation", func(t *testing.T) {
		entered := make(chan struct{})
		x := startLifecycle(t, 5000, func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			return errors.New("independent oracle failure")
		})
		x.query(t, "COMMIT")
		x.reply(t, "COMMIT", 'I')
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("oracle was not entered")
		}
		x.p.Close()
		x.assertEvidence(t, "proxy_shutdown", "independent oracle failure", "no_pending_transaction", "I")
	})
}
