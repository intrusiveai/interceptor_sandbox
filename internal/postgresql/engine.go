package postgresql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
	"github.com/jackc/pgx/v5"
)

// Engine state and its Unix socket are under a supervisor-only directory. The
// application has no connection path except Proxy; no backend TCP is enabled.
type Engine struct {
	ID       string
	Config   Config
	Root     string
	Bin      string
	Blobs    *blobstore.Store
	Recorder *recording.Writer
	Journal  *statejournal.Writer
	command  *exec.Cmd
	done     chan error
	Failure  func(error)
	stop     chan struct{}
	stopOnce sync.Once
	admin    *pgx.Conn
	adminMu  sync.Mutex
	gate     sync.RWMutex
}

func (e *Engine) socket() string { return filepath.Join(e.Root, "socket") }
func (e *Engine) connection(ctx context.Context, role string) (*pgx.Conn, error) {
	c, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	c.Host = e.socket()
	c.Port = 5432
	c.Database = e.Config.Database
	c.User = role
	c.Password = ""
	c.TLSConfig = nil
	c.Fallbacks = nil
	c.ConnectTimeout = 5 * time.Second
	c.DefaultQueryExecMode = pgx.QueryExecModeExec
	return pgx.ConnectConfig(ctx, c)
}
func (e *Engine) commandFor(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, filepath.Join(e.Bin, name), args...)
	cmd.Env = []string{"PATH=" + e.Bin + ":/usr/bin:/bin", "LC_ALL=C", "HOME=" + e.Root, "TMPDIR=" + e.Root, "PGHOST=" + e.socket(), "PGPORT=5432", "PGUSER=interceptor_admin", "PGDATABASE=" + e.Config.Database}
	return cmd
}
func (e *Engine) Start(ctx context.Context, assets string, restore *statejournal.Operation) (err error) {
	if e.Bin == "" {
		e.Bin = Prefix + "/bin"
	}
	if err = e.Config.Validate(); err != nil {
		return err
	}
	if e.Recorder == nil || e.Blobs == nil || e.Journal == nil {
		return errors.New("database requires native evidence sinks")
	}
	if len(e.socket()) > 90 {
		return errors.New("database Unix socket path exceeds supported length")
	}
	if err = os.MkdirAll(e.Root, 0700); err != nil {
		return err
	}
	if err = os.Chmod(e.Root, 0700); err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(e.Root, "data")); err == nil {
		return errors.New("database runtime already exists; refusing implicit reuse")
	}
	if err = os.Mkdir(e.socket(), 0700); err != nil {
		return err
	}
	init := e.commandFor(ctx, "initdb", "-D", filepath.Join(e.Root, "data"), "-U", "interceptor_admin", "--auth-local=trust", "--auth-host=reject", "--encoding=UTF8", "--locale=C")
	if out, x := init.CombinedOutput(); x != nil {
		return fmt.Errorf("initialize PostgreSQL: %w: %s", x, out)
	}
	// Only management roles use trust, on the private Unix socket. Every
	// application role must authenticate with PostgreSQL's native SCRAM server.
	hba := "local all interceptor_admin trust\nlocal all interceptor_oracle trust\nlocal all all scram-sha-256\nhost all all all reject\n"
	if err = os.WriteFile(filepath.Join(e.Root, "data", "pg_hba.conf"), []byte(hba), 0600); err != nil {
		return err
	}
	args := []string{"-D", filepath.Join(e.Root, "data"), "-k", e.socket(), "-h", "", "-p", "5432", "-c", "unix_socket_permissions=0700", "-c", "max_connections=" + strconv.Itoa(e.Config.MaxConnections+8), "-c", "shared_buffers=16MB", "-c", "password_encryption=scram-sha-256", "-c", "statement_timeout=" + strconv.Itoa(e.Config.QueryTimeoutMS), "-c", "lock_timeout=" + strconv.Itoa(e.Config.QueryTimeoutMS), "-c", "idle_in_transaction_session_timeout=" + strconv.Itoa(e.Config.QueryTimeoutMS), "-c", "max_prepared_transactions=0", "-c", "max_wal_size=64MB", "-c", "min_wal_size=32MB", "-c", "log_statement=none", "-c", "log_min_error_statement=panic"}
	e.command = e.commandFor(context.Background(), "postgres", args...)
	log, x := os.OpenFile(filepath.Join(e.Root, "postgres.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if x != nil {
		return x
	}
	e.command.Stdout = &limitedLog{writer: log, remaining: 1 << 20}
	e.command.Stderr = e.command.Stdout
	if err = e.command.Start(); err != nil {
		log.Close()
		return err
	}
	e.done = make(chan error, 1)
	e.stop = make(chan struct{})
	go func() {
		err := e.command.Wait()
		log.Close()
		e.done <- err
		select {
		case <-e.stop:
		default:
			if e.Failure != nil {
				e.Failure(fmt.Errorf("database %s backend exited unexpectedly: %v", e.ID, err))
			}
		}
	}()
	defer func() {
		if err != nil {
			e.Close()
		}
	}()
	// The application database does not exist during initial readiness.
	name := e.Config.Database
	e.Config.Database = "postgres"
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.admin, err = e.connection(ctx, "interceptor_admin")
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			e.Config.Database = name
			return fmt.Errorf("PostgreSQL readiness: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.Config.Database = name
	_, err = e.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	if err != nil {
		return err
	}
	for _, r := range e.Config.Roles {
		// Passwords are parameters, used by server-side format, never command argv.
		var sql string
		if err = e.admin.QueryRow(ctx, "SELECT format('CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', $1::text,$2::text)", r.Name, r.Password).Scan(&sql); err != nil {
			return err
		}
		if _, err = e.admin.Exec(ctx, sql); err != nil {
			return fmt.Errorf("create database application role: %w", err)
		}
	}
	if _, err = e.admin.Exec(ctx, "CREATE ROLE interceptor_oracle NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
		return err
	}
	e.admin.Close(ctx)
	e.admin, err = e.connection(ctx, "interceptor_admin")
	if err != nil {
		return err
	}
	if restore != nil {
		if err = ValidateCheckpoint(*restore); err != nil {
			return err
		}
		reader, _, x := e.Blobs.Open(restore.Blob)
		if x != nil {
			return x
		}
		defer reader.Close()
		cmd := e.commandFor(ctx, "pg_restore", "--exit-on-error", "--single-transaction", "--dbname", name)
		cmd.Stdin = reader
		if out, x := cmd.CombinedOutput(); x != nil {
			return fmt.Errorf("restore PostgreSQL: %w: %s", x, out)
		}
	} else {
		if _, err = e.admin.Exec(ctx, "CREATE EXTENSION vector"); err != nil {
			return err
		}
		for _, asset := range append(append([]string{}, e.Config.Schema...), e.Config.Seeds...) {
			data, x := os.ReadFile(filepath.Join(assets, asset))
			if x != nil {
				return x
			}
			// Exec without arguments uses PostgreSQL's simple-query protocol; no psql
			// metacommand or shell evaluation is involved.
			if _, err = e.admin.Exec(ctx, string(data)); err != nil {
				return fmt.Errorf("database initialization asset %s: %w", asset, err)
			}
		}
	}
	var nativeVersion int
	if err = e.admin.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&nativeVersion); err != nil || nativeVersion != 180006 {
		return errors.New("packaged PostgreSQL version mismatch")
	}
	var version string
	if err = e.admin.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname='vector'").Scan(&version); err != nil || version != VectorVersion {
		return fmt.Errorf("packaged pgvector version mismatch: %s (%v)", version, err)
	}
	for _, r := range e.Config.Roles {
		var safe bool
		err = e.admin.QueryRow(ctx, "SELECT NOT (rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls) AND NOT EXISTS (SELECT 1 FROM pg_auth_members WHERE member=r.oid) FROM pg_roles r WHERE rolname=$1", r.Name).Scan(&safe)
		if err != nil || !safe {
			return fmt.Errorf("application role %s has forbidden privileges", r.Name)
		}
	}
	// Oracle queries are sealed but still run with read-only access.
	rows, x := e.admin.Query(ctx, "SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg_%' AND nspname <> 'information_schema'")
	if x != nil {
		return x
	}
	var schemas []string
	for rows.Next() {
		var n string
		if x = rows.Scan(&n); x != nil {
			rows.Close()
			return x
		}
		schemas = append(schemas, n)
	}
	rows.Close()
	if x = rows.Err(); x != nil {
		return x
	}
	for _, n := range schemas {
		q := pgx.Identifier{n}.Sanitize()
		if _, err = e.admin.Exec(ctx, "GRANT USAGE ON SCHEMA "+q+" TO interceptor_oracle; GRANT SELECT ON ALL TABLES IN SCHEMA "+q+" TO interceptor_oracle"); err != nil {
			return err
		}
	}
	// Check storage while SQL runs as well as at admission. The watchdog stops
	// the service on excess; it is a bounded overshoot check, not a disk quota.
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-ticker.C:
				if err := e.Check(context.Background()); err != nil {
					if e.Failure != nil {
						e.Failure(err)
					}
					_ = e.command.Process.Kill()
					return
				}
			}
		}
	}()
	return e.Record(ctx, "database_ready", recording.SandboxControl, map[string]any{"engine": Version, "pgvector": version, "restored": restore != nil}, nil)
}
func (e *Engine) Record(ctx context.Context, kind string, provenance recording.Provenance, semantic any, raw []byte) error {
	if err := e.Recorder.Err(); err != nil {
		return err
	}
	ref := ""
	if raw != nil {
		o, err := e.Blobs.Put(ctx, bytes.NewReader(raw))
		if err != nil {
			e.Recorder.Fail(err)
			return e.Recorder.Err()
		}
		ref = o.Digest
	}
	attemptID, payloadDigest := "", ""
	if provenance == recording.HarnessInjected {
		if detail, ok := semantic.(map[string]any); ok {
			attemptID, _ = detail["attempt_id"].(string)
			payloadDigest, _ = detail["payload_digest"].(string)
		}
	}
	_, err := e.Recorder.AppendContext(ctx, recording.Draft{AttemptID: attemptID, PayloadDigest: payloadDigest, Channel: recording.Evaluation, Layer: "database", Kind: kind, Provenance: provenance, Semantic: map[string]any{"service": e.ID, "detail": semantic}, RawRef: ref})
	if err == nil {
		err = e.Recorder.Flush()
	}
	return err
}
func (e *Engine) Close() error {
	e.stopOnce.Do(func() {
		if e.stop != nil {
			close(e.stop)
		}
	})
	if e.admin != nil {
		e.admin.Close(context.Background())
	}
	if e.command == nil || e.command.Process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := e.commandFor(ctx, "pg_ctl", "-D", filepath.Join(e.Root, "data"), "-m", "fast", "-w", "stop")
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = e.command.Process.Kill()
	}
	if e.done != nil {
		<-e.done
		e.done = nil
	}
	if err != nil {
		return fmt.Errorf("stop PostgreSQL: %w: %s", err, out)
	}
	return nil
}
func (e *Engine) Check(ctx context.Context) error {
	if err := e.Recorder.Err(); err != nil {
		return err
	}
	var size int64
	err := filepath.WalkDir(filepath.Join(e.Root, "data"), func(path string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			i, x := d.Info()
			if os.IsNotExist(x) {
				return nil
			}
			if x != nil {
				return x
			}
			size += i.Size()
		}
		if size > e.Config.MaxStorageBytes {
			return errors.New("database storage limit exceeded")
		}
		return ctx.Err()
	})
	return err
}
func (e *Engine) Backup(ctx context.Context) (statejournal.Operation, error) {
	e.adminMu.Lock()
	defer e.adminMu.Unlock()
	var active int
	err := e.admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND (state <> 'idle' OR xact_start IS NOT NULL)").Scan(&active)
	if err != nil {
		return statejournal.Operation{}, err
	}
	if active != 0 {
		return statejournal.Operation{}, errors.New("database snapshot requires drained transactions; finish or roll back open transactions")
	}
	cmd := e.commandFor(ctx, "pg_dump", "--format=custom", "--compress=0", "--dbname", e.Config.Database)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return statejournal.Operation{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return statejournal.Operation{}, err
	}
	object, putErr := e.Blobs.Put(ctx, pipe)
	if putErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if putErr != nil {
		e.Recorder.Fail(putErr)
		return statejournal.Operation{}, e.Recorder.Err()
	}
	if waitErr != nil {
		return statejournal.Operation{}, fmt.Errorf("database backup: %w: %s", waitErr, stderr.String())
	}
	return statejournal.Operation{Kind: "database_checkpoint", Service: e.ID, Blob: object.Digest, Size: object.Size, Value: checkpointValue(nil)}, nil
}

// Keep the gate separate from supervisor activity: save_state hooks may query
// PostgreSQL while holding the supervisor's snapshot/activity gate.
func (e *Engine) Freeze(ctx context.Context) (func(), error) {
	for !e.gate.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return e.gate.Unlock, nil
}
func (e *Engine) Oracle(ctx context.Context, sql string) (json.RawMessage, error) {
	e.adminMu.Lock()
	defer e.adminMu.Unlock()
	tx, err := e.admin.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SET LOCAL ROLE interceptor_oracle"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []any{}
	for rows.Next() {
		v, x := rows.Values()
		if x != nil {
			return nil, x
		}
		values = append(values, v)
		if len(values) > 10000 {
			return nil, errors.New("oracle result exceeds row limit")
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > e.Config.MaxResultBytes {
		return nil, errors.New("oracle result exceeds byte limit")
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return encoded, nil
}

// Checkpoint metadata shares the journal's existing integrity envelope.
type CheckpointMetadata struct {
	Version         string                     `json:"version"`
	OracleBaselines map[string]json.RawMessage `json:"oracle_baselines,omitempty"`
}

func checkpointValue(baselines map[string]json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(CheckpointMetadata{Version: Version + "/" + VectorVersion, OracleBaselines: baselines})
	return b
}
func WithOracleBaselines(op statejournal.Operation, baselines map[string]json.RawMessage) statejournal.Operation {
	op.Value = checkpointValue(baselines)
	return op
}
func ValidateCheckpoint(op statejournal.Operation) error {
	var m CheckpointMetadata
	if op.Kind != "database_checkpoint" || json.Unmarshal(op.Value, &m) != nil || m.Version != Version+"/"+VectorVersion {
		return errors.New("database checkpoint engine version mismatch")
	}
	return nil
}

type limitedLog struct {
	writer    io.Writer
	remaining int
}

func (l *limitedLog) Write(b []byte) (int, error) {
	n := len(b)
	if len(b) > l.remaining {
		b = b[:l.remaining]
	}
	if len(b) > 0 {
		written, err := l.writer.Write(b)
		l.remaining -= written
		if err != nil {
			return written, err
		}
	}
	return n, nil
}
