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
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
	"github.com/jackc/pgx/v5"
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
	fresh := &Engine{ID: "database", Config: cfg, Root: filepath.Join(root, "fresh"), Bin: e.Bin, Recorder: rec, Journal: journal, Blobs: blobs}
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
}
