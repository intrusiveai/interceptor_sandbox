package postgresql

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

func testTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "database.test"}, DNSNames: []string{"database.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, _ := x509.MarshalECPrivateKey(key)
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	ca := x509.NewCertPool()
	ca.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, &tls.Config{RootCAs: ca, ServerName: "database.test", MinVersion: tls.VersionTLS12}
}
func TestNativePostgreSQL(t *testing.T) {
	if os.Getenv("INTERCEPTOR_TEST_POSTGRES_BIN") == "" {
		t.Skip("set INTERCEPTOR_TEST_POSTGRES_BIN to exercise packaged native engine")
	}
	root, err := os.MkdirTemp("/tmp", "ipg-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rec, err := recording.Open(filepath.Join(root, "events"), "database-test")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	journal, err := statejournal.Open(filepath.Join(root, "journal"), "database-test")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	journal.SetFailureHandler(rec.Fail)
	blobs, err := blobstore.Open(filepath.Join(root, "blobs"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	schema := `CREATE TABLE documents(id text PRIMARY KEY, body text NOT NULL, metadata jsonb NOT NULL, embedding vector(3)); GRANT SELECT, INSERT, UPDATE, DELETE ON documents TO agent;`
	seeds := `INSERT INTO documents VALUES ('one','benign','{"source":"seed"}','[1,0,0]'),('two','second','{}','[0,1,0]');`
	os.WriteFile(filepath.Join(root, "schema.sql"), []byte(schema), 0600)
	os.WriteFile(filepath.Join(root, "seed.sql"), []byte(seeds), 0600)
	cfg := Config{Database: "support", Roles: []Role{{Name: "agent", Password: "synthetic-password"}}, Schema: []string{"schema.sql"}, Seeds: []string{"seed.sql"}, Tables: []Table{{ID: "documents", Schema: "public", Name: "documents", Keys: []string{"id"}, Fields: []string{"body", "metadata", "embedding"}, AllowCreate: true, MaxRecords: 10}}}
	cfg.Defaults()
	cfg.MaxConnections = 2
	e := &Engine{ID: "database", Config: cfg, Root: filepath.Join(root, "engine"), Bin: os.Getenv("INTERCEPTOR_TEST_POSTGRES_BIN"), Recorder: rec, Journal: journal, Blobs: blobs}
	if err = e.Start(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	manager := injection.NewManager(journal, rec)
	serverTLS, clientTLS := testTLS(t)
	p := &Proxy{Engine: e, TLS: serverTLS, Injections: manager}
	if err = p.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	connect := func() *pgx.Conn {
		t.Helper()
		c, _ := pgx.ParseConfig("")
		c.Host = "127.0.0.1"
		c.Port = uint16(p.listener.Addr().(*net.TCPAddr).Port)
		c.Database = cfg.Database
		c.User = "agent"
		c.Password = "synthetic-password"
		c.TLSConfig = clientTLS
		c.Fallbacks = nil
		c.ConnectTimeout = 5 * time.Second
		conn, x := pgx.ConnectConfig(ctx, c)
		if x != nil {
			t.Fatal(x)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		return conn
	}
	a := connect()
	for _, mode := range []string{"wrong-password", "wrong-hostname", "management-role"} {
		bad := a.Config().Copy()
		bad.TLSConfig = bad.TLSConfig.Clone()
		switch mode {
		case "wrong-password":
			bad.Password = "invalid-password"
		case "wrong-hostname":
			bad.TLSConfig.ServerName = "elsewhere.test"
		case "management-role":
			bad.User = "interceptor_admin"
		}
		conn, x := pgx.ConnectConfig(ctx, bad)
		if x == nil {
			conn.Close(ctx)
			t.Fatalf("accepted %s", mode)
		}
	}
	b := connect()
	if extra, x := pgx.ConnectConfig(ctx, a.Config().Copy()); x == nil {
		extra.Close(ctx)
		t.Fatal("connection ceiling ignored")
	}
	queryDone := make(chan error, 1)
	go func() { _, err := a.Exec(ctx, "SELECT pg_sleep(10)"); queryDone <- err }()
	time.Sleep(100 * time.Millisecond)
	if err = a.PgConn().CancelRequest(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-queryDone; err == nil {
		t.Fatal("full-pool cancellation did not cancel query")
	}
	// Exercise native simple-query multiple results and common SQL semantics.
	results, err := a.PgConn().Exec(ctx, `SELECT id FROM documents WHERE id='one' UNION SELECT id FROM documents WHERE id='two' ORDER BY id LIMIT 1; SELECT count(*) FROM documents GROUP BY metadata->>'source' ORDER BY count(*)`).ReadAll()
	if err != nil || len(results) != 2 || len(results[0].Rows) != 1 || string(results[0].Rows[0][0]) != "one" || len(results[1].Rows) != 2 {
		t.Fatalf("multiple SQL results: %#v %v", results, err)
	}
	for _, format := range []int16{0, 1} {
		for column, pointer := range map[string]string{"text": "/rows/0/0", "json": "/rows/0/1/body"} {
			d := injection.Definition{ID: fmt.Sprintf("typed-%d-%s", format, column), Surface: "database_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "database", CallOrdinal: "first", RequestBodyPointer: "/query", RequestBodyContain: "typed_result"}, Placement: injection.Placement{Operation: "append_text", Pointer: pointer}, Payload: json.RawMessage(`" injected"`)}
			if err = manager.Arm(d, ""); err != nil {
				t.Fatal(err)
			}
		}
		rows, x := a.Query(ctx, `SELECT 'original'::text AS typed_result, '{"body":"old"}'::jsonb, NULL::text, 42::int4`, pgx.QueryResultFormats{format, format, format, format})
		if x != nil {
			t.Fatal(x)
		}
		if !rows.Next() {
			t.Fatal("typed result absent", rows.Err())
		}
		values, x := rows.Values()
		rows.Close()
		if x != nil || values[0] != "original injected" || values[1].(map[string]any)["body"] != "old injected" || values[2] != nil || values[3] != int32(42) {
			t.Fatalf("typed result format %d: %#v %v", format, values, x)
		}
	}
	// Real extended-protocol portal pagination, pipelined within each Sync
	// group. More than 256 successive transactions must not retain dead portals.
	frontend := a.PgConn().Frontend()
	for i := 0; i < 260; i++ {
		name := fmt.Sprintf("portal-%d", i)
		frontend.SendParse(&pgproto3.Parse{Query: "SELECT generate_series(1,2)::text AS item"})
		frontend.SendBind(&pgproto3.Bind{DestinationPortal: name})
		frontend.SendDescribe(&pgproto3.Describe{ObjectType: 'P', Name: name})
		frontend.SendExecute(&pgproto3.Execute{Portal: name, MaxRows: 1})
		frontend.SendExecute(&pgproto3.Execute{Portal: name})
		frontend.SendSync(&pgproto3.Sync{})
		if err = frontend.Flush(); err != nil {
			t.Fatal(err)
		}
		count, suspended := 0, false
	readPortal:
		for {
			message, x := frontend.Receive()
			if x != nil {
				t.Fatal(x)
			}
			switch m := message.(type) {
			case *pgproto3.ErrorResponse:
				t.Fatalf("portal %d: %#v", i, m)
			case *pgproto3.DataRow:
				count++
				if string(m.Values[0]) != fmt.Sprint(count) {
					t.Fatal("portal result ordering changed")
				}
			case *pgproto3.PortalSuspended:
				suspended = true
			case *pgproto3.ReadyForQuery:
				break readPortal
			}
		}
		if count != 2 || !suspended {
			t.Fatal("partial portal fetch missing")
		}
	}
	var body string
	if err = a.QueryRow(ctx, "SELECT body FROM documents WHERE id=$1", "one").Scan(&body); err != nil || body != "benign" {
		t.Fatalf("seed: %s %v", body, err)
	}
	var nearest string
	if err = a.QueryRow(ctx, "SELECT id FROM documents ORDER BY embedding <-> '[1,0,0]' LIMIT 1").Scan(&nearest); err != nil || nearest != "one" {
		t.Fatalf("vector: %s %v", nearest, err)
	}
	tx, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE documents SET body='uncommitted' WHERE id='one'"); err != nil {
		t.Fatal(err)
	}
	if err = b.QueryRow(ctx, "SELECT body FROM documents WHERE id='one'").Scan(&body); err != nil || body != "benign" {
		t.Fatalf("independent transaction: %s %v", body, err)
	}
	if _, err = e.Backup(ctx); err == nil {
		t.Fatal("snapshot accepted active transaction")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Native SQL errors must recover at Sync without poisoning prepared metadata.
	if _, err = a.Prepare(ctx, "duplicate", "SELECT body FROM documents WHERE id=$1"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Prepare(ctx, "duplicate", "SELECT 1/0"); err == nil {
		t.Fatal("duplicate prepared name accepted")
	}
	if err = a.QueryRow(ctx, "duplicate", "one").Scan(&body); err != nil || body != "benign" {
		t.Fatalf("prepared recovery: %q %v", body, err)
	}
	if _, err = a.Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("SQL error absent")
	}
	if err = a.QueryRow(ctx, "SELECT body FROM documents WHERE id='one'").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Exec(ctx, "BEGIN; SAVEPOINT safe; UPDATE documents SET body='temporary'; ROLLBACK TO safe; COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err = b.QueryRow(ctx, "SELECT body FROM documents WHERE id='one'").Scan(&body); err != nil || body != "benign" {
		t.Fatal("savepoint rollback failed")
	}
	d := injection.Definition{ID: "response", Surface: "database_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "database", CallOrdinal: "first", RequestBodyPointer: "/query", RequestBodyContain: "SELECT body"}, Placement: injection.Placement{Operation: "append_text", Pointer: "/rows/0/0"}, Payload: json.RawMessage(`" injected"`)}
	if err = manager.Arm(d, ""); err != nil {
		t.Fatal(err)
	}
	if err = a.QueryRow(ctx, "SELECT body FROM documents WHERE id=$1", "one").Scan(&body); err != nil || body != "benign injected" {
		t.Fatalf("response: %q %v", body, err)
	}
	if err = b.QueryRow(ctx, "SELECT body FROM documents WHERE id='one'").Scan(&body); err != nil || body != "benign" {
		t.Fatalf("response mutated stored row: %q %v", body, err)
	}
	d.ID = "state"
	d.Surface = "environment_state"
	d.Mode = "fidelity"
	d.Selector = injection.Selector{Service: "database", Collection: "documents", Key: `{"id":"one"}`}
	d.Placement = injection.Placement{Operation: "append_text", Pointer: "/body"}
	if err = e.Mutate(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err = b.QueryRow(ctx, "SELECT body FROM documents WHERE id='one'").Scan(&body); err != nil || body != "benign injected" {
		t.Fatalf("state: %s %v", body, err)
	}
	create := d
	create.ID = "create"
	create.Selector.Key = `{"id":"three"}`
	create.Placement = injection.Placement{Operation: "replace"}
	create.Payload = json.RawMessage(`{"id":"three","body":"created","metadata":{},"embedding":"[0,0,1]"}`)
	if err = e.Mutate(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err = e.Mutate(ctx, create); err == nil {
		t.Fatal("creation silently upserted")
	}
	invalid := create
	invalid.Selector.Key = `{"id":"invalid"}`
	invalid.Payload = json.RawMessage(`{"id":"invalid","body":"bad","metadata":{},"embedding":"[1,0]"}`)
	if err = e.Mutate(ctx, invalid); err == nil {
		t.Fatal("invalid vector dimensions accepted")
	}
	var count int
	if err = b.QueryRow(ctx, "SELECT count(*) FROM documents").Scan(&count); err != nil || count != 3 {
		t.Fatalf("atomic creation: %d %v", count, err)
	}
	originalSeed, _ := os.ReadFile(filepath.Join(root, "seed.sql"))
	if string(originalSeed) != seeds {
		t.Fatal("seed source changed")
	}
	unlock, err := e.Freeze(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := e.Backup(ctx)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	a.Close(ctx)
	b.Close(ctx)
	p.Close()
	restored := &Engine{ID: "database", Config: cfg, Root: filepath.Join(root, "restored"), Bin: e.Bin, Recorder: rec, Journal: journal, Blobs: blobs}
	if err = restored.Start(ctx, root, &backup); err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	value, err := restored.Oracle(ctx, "SELECT body FROM documents WHERE id='one'")
	if err != nil || string(value) != `[["benign injected"]]` {
		t.Fatalf("restore: %s %v", value, err)
	}
	resourceFailure := make(chan error, 1)
	fresh := &Engine{ID: "database", Config: cfg, Root: filepath.Join(root, "fresh"), Bin: e.Bin, Recorder: rec, Journal: journal, Blobs: blobs,
		Failure: func(err error) { resourceFailure <- err }}
	if err = fresh.Start(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	value, err = fresh.Oracle(ctx, "SELECT body FROM documents ORDER BY id")
	if err != nil || string(value) != `[["benign"],["second"]]` {
		t.Fatalf("fresh session contaminated: %s %v", value, err)
	}
	events, _ := os.ReadFile(filepath.Join(root, "events"))
	if bytes.Contains(events, []byte("synthetic-password")) || bytes.Contains(events, []byte("invalid-password")) {
		t.Fatal("authentication secret recorded")
	}
	if _, err = recording.VerifyFile(filepath.Join(root, "events")); err != nil {
		t.Fatal(err)
	}
	t.Run("evidence failure does not kill the control path", func(t *testing.T) {
		rec.Fail(errors.New("test capture storage unavailable"))
		if err := fresh.Check(ctx); err == nil || !strings.Contains(err.Error(), "evidence_storage_error") {
			t.Fatalf("database work was not blocked: %v", err)
		}
		// Let several watchdog ticks pass: an evidence failure must not trigger
		// the fatal resource callback before the supervisor can report it.
		select {
		case err := <-resourceFailure:
			t.Fatalf("evidence error incorrectly escalated as resource failure: %v", err)
		case <-time.After(350 * time.Millisecond):
		}
		if err := fresh.admin.Ping(ctx); err != nil {
			t.Fatalf("evidence failure killed PostgreSQL: %v", err)
		}
		// Storage enforcement must still work after capture has failed.
		path := filepath.Join(fresh.Root, "data", "test-storage-limit")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		err = file.Truncate(fresh.Config.MaxStorageBytes + 1)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-resourceFailure:
			if !strings.Contains(err.Error(), "database storage limit exceeded") {
				t.Fatalf("wrong resource failure: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("storage watchdog stopped enforcing its limit")
		}
	})
}
