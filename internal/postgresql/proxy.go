package postgresql

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/jackc/pgx/v5/pgproto3"
)

type Proxy struct {
	After      func(context.Context) error
	Engine     *Engine
	TLS        *tls.Config
	Injections *injection.Manager
	Observe    func(context.Context) (context.Context, func())
	Failure    func(error)
	listener   net.Listener
	mu         sync.Mutex
	clients    map[net.Conn]bool
	keys       map[uint32][]byte
	next       atomic.Uint64
	wg         sync.WaitGroup
}

func (p *Proxy) Start(address string) error {
	if p.TLS == nil || len(p.TLS.Certificates) == 0 {
		return errors.New("PostgreSQL proxy requires TLS")
	}
	l, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	p.listener = l
	p.clients = map[net.Conn]bool{}
	p.keys = map[uint32][]byte{}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if len(p.clients) >= p.Engine.Config.MaxConnections {
				p.mu.Unlock()
				c.Close()
				continue
			}
			p.clients[c] = true
			p.wg.Add(1)
			p.mu.Unlock()
			go func() {
				defer p.wg.Done()
				defer func() { c.Close(); p.mu.Lock(); delete(p.clients, c); p.mu.Unlock() }()
				if err := p.serve(c); err != nil && p.Engine.Recorder.Err() != nil && p.Failure != nil {
					p.Failure(p.Engine.Recorder.Err())
				}
			}()
		}
	}()
	return nil
}
func (p *Proxy) Close() error {
	if p.listener == nil {
		return nil
	}
	p.listener.Close()
	p.mu.Lock()
	for c := range p.clients {
		c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}
func send(w io.Writer, m interface{ Encode([]byte) ([]byte, error) }) error {
	b, err := m.Encode(nil)
	if err != nil {
		return err
	}
	for len(b) > 0 {
		n, x := w.Write(b)
		if x != nil {
			return x
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func fatal(c net.Conn, err error) error {
	_ = send(c, &pgproto3.ErrorResponse{Severity: "FATAL", Code: "58000", Message: err.Error()})
	return err
}
func (p *Proxy) serve(raw net.Conn) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	c := raw
	b := pgproto3.NewBackend(c, c)
	b.SetMaxBodyLen(1 << 20)
	start, err := b.ReceiveStartupMessage()
	if err != nil {
		return err
	}
	if request, ok := start.(*pgproto3.CancelRequest); ok {
		return p.cancel(request)
	}
	if _, ok := start.(*pgproto3.SSLRequest); !ok {
		return fatal(c, errors.New("PostgreSQL TLS is required; configure sslmode=verify-full and the sandbox CA"))
	}
	if _, err = c.Write([]byte{'S'}); err != nil {
		return err
	}
	secured := tls.Server(c, p.TLS.Clone())
	if err = secured.HandshakeContext(ctx); err != nil {
		return err
	}
	c = secured
	b = pgproto3.NewBackend(c, c)
	b.SetMaxBodyLen(1 << 20)
	start, err = b.ReceiveStartupMessage()
	if err != nil {
		return err
	}
	startup, ok := start.(*pgproto3.StartupMessage)
	if !ok {
		return fatal(c, errors.New("unsupported PostgreSQL startup"))
	}
	allowed := false
	for _, r := range p.Engine.Config.Roles {
		if startup.Parameters["user"] == r.Name {
			allowed = true
		}
	}
	if !allowed || startup.Parameters["database"] != p.Engine.Config.Database || startup.Parameters["replication"] != "" {
		return fatal(c, errors.New("undeclared PostgreSQL role, database, or replication connection"))
	}
	upstream, err := net.DialTimeout("unix", p.Engine.socket()+"/.s.PGSQL.5432", 5*time.Second)
	if err != nil {
		return fatal(c, errors.New("private PostgreSQL is unavailable"))
	}
	defer upstream.Close()
	upstream.SetDeadline(time.Now().Add(10 * time.Second))
	f := pgproto3.NewFrontend(upstream, upstream)
	f.SetMaxBodyLen(int(p.Engine.Config.MaxResultBytes))
	// Native PostgreSQL on the private non-TLS socket advertises SCRAM-SHA-256,
	// not PLUS. A client requiring channel binding fails instead of downgrading.
	if err = send(upstream, startup); err != nil {
		return err
	}
	var pid uint32
	for {
		m, x := f.Receive()
		if x != nil {
			return x
		}
		if k, ok := m.(*pgproto3.BackendKeyData); ok {
			pid = k.ProcessID
			p.mu.Lock()
			p.keys[pid] = append([]byte(nil), k.SecretKey...)
			p.mu.Unlock()
			defer func() { p.mu.Lock(); delete(p.keys, pid); p.mu.Unlock() }()
		}
		if err = send(c, m); err != nil {
			return err
		}
		switch m.(type) {
		case *pgproto3.ErrorResponse:
			return errors.New("PostgreSQL authentication failed")
		case *pgproto3.ReadyForQuery:
			goto authenticated
		case *pgproto3.AuthenticationSASL:
			b.SetAuthType(pgproto3.AuthTypeSASL)
		case *pgproto3.AuthenticationSASLContinue:
			b.SetAuthType(pgproto3.AuthTypeSASLContinue)
		default:
			continue
		}
		reply, x := b.Receive()
		if x != nil {
			return x
		}
		if err = send(upstream, reply); err != nil {
			return err
		}
	}
authenticated:
	c.SetDeadline(time.Time{})
	upstream.SetDeadline(time.Time{})
	connID := p.next.Add(1)
	state := newConnection(p, connID)
	requests := make(chan *execution, 128)
	errorsCh := make(chan error, 1)
	go func() { err := state.forward(ctx, b, upstream, requests); errorsCh <- err; cancel(); upstream.Close() }()
	err = state.responses(ctx, f, c, requests)
	cancel()
	upstream.Close()
	// Unblock a reader without closing the TLS write side yet.
	raw.SetReadDeadline(time.Now())
	forwardErr := <-errorsCh
	if forwardErr != nil && !errors.Is(forwardErr, io.EOF) && !errors.Is(forwardErr, context.Canceled) {
		if n, ok := forwardErr.(net.Error); !ok || !n.Timeout() {
			err = forwardErr
		}
	}
	if state.timedOut.Load() {
		err = errors.New("PostgreSQL query exceeded query_timeout_ms; connection closed, transaction outcome unconfirmed")
	}
	_ = p.Engine.Record(context.Background(), "database_connection_closed", recording.SandboxControl, map[string]any{"connection": connID, "last_observed_transaction_status": string([]byte{state.txStatus}), "rollback_confirmed": false}, nil)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = p.Engine.Record(context.Background(), "database_connection_failed", recording.SandboxControl, map[string]any{"connection": connID, "reason": err.Error(), "transaction_outcome": "unconfirmed"}, nil)
		_ = fatal(c, err)
	}
	c.Close()
	// Finish queued observations even when a connection fails mid-pipeline.
	for r := range requests {
		r.finish()
	}
	return err
}
func (p *Proxy) cancel(request *pgproto3.CancelRequest) error {
	p.mu.Lock()
	key, ok := p.keys[request.ProcessID]
	p.mu.Unlock()
	if !ok || string(key) != string(request.SecretKey) {
		return errors.New("unknown PostgreSQL cancellation key")
	}
	c, err := net.DialTimeout("unix", p.Engine.socket()+"/.s.PGSQL.5432", time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	return send(c, request)
}

type statement struct {
	sql     string
	types   []uint32
	columns []pgproto3.FieldDescription
}
type portal struct {
	statement     *statement
	params        [][]byte
	formats       []int16
	resultFormats []int16
}
type execution struct {
	kind      byte
	sql       string
	portal    *portal
	origin    *portal
	statement *statement
	ctx       context.Context
	done      func()
	once      sync.Once
	id        uint64
	undo      func()
	barrier   chan struct{}
}

func (e *execution) finish() {
	e.once.Do(func() {
		if e.done != nil {
			e.done()
		}
	})
}

type connection struct {
	p          *Proxy
	id         uint64
	next       uint64
	timedOut   atomic.Bool
	txStatus   byte
	mu         sync.Mutex
	statements map[string]*statement
	portals    map[string]*portal
	cursors    map[string]cursor
}

func newConnection(p *Proxy, id uint64) *connection {
	return &connection{p: p, id: id, txStatus: 'I', statements: map[string]*statement{}, portals: map[string]*portal{}, cursors: map[string]cursor{}}
}
func cloneFields(fields []pgproto3.FieldDescription) []pgproto3.FieldDescription {
	copied := append([]pgproto3.FieldDescription(nil), fields...)
	for i := range copied {
		copied[i].Name = append([]byte(nil), copied[i].Name...)
	}
	return copied
}
func cloneBytes(v [][]byte) [][]byte {
	r := make([][]byte, len(v))
	for i, b := range v {
		if b != nil {
			r[i] = append([]byte{}, b...)
		}
	}
	return r
}
func (s *connection) forward(ctx context.Context, b *pgproto3.Backend, up net.Conn, q chan<- *execution) error {
	defer close(q)
	for {
		msg, err := b.Receive()
		if err != nil {
			return err
		}
		r := &execution{ctx: ctx}
		s.mu.Lock()
		switch m := msg.(type) {
		case *pgproto3.Query:
			r.kind = 'Q'
			r.sql = m.String
		case *pgproto3.Parse:
			r.kind = 'P'
			r.sql = m.Query
			r.statement = &statement{sql: m.Query, types: append([]uint32(nil), m.ParameterOIDs...)}
			name := m.Name
			old, exists := s.statements[name]
			r.undo = func() {
				if exists {
					s.statements[name] = old
				} else {
					delete(s.statements, name)
				}
			}
			s.statements[name] = r.statement
		case *pgproto3.Bind:
			r.kind = 'B'
			st := s.statements[m.PreparedStatement]
			r.portal = &portal{statement: st, params: cloneBytes(m.Parameters), formats: append([]int16(nil), m.ParameterFormatCodes...), resultFormats: append([]int16(nil), m.ResultFormatCodes...)}
			name := m.DestinationPortal
			old, exists := s.portals[name]
			r.undo = func() {
				if exists {
					s.portals[name] = old
				} else {
					delete(s.portals, name)
				}
			}
			s.portals[name] = r.portal
		case *pgproto3.Describe:
			r.kind = 'D'
			if m.ObjectType == 'S' {
				r.statement = s.statements[m.Name]
			} else {
				r.portal = s.portals[m.Name]
			}
		case *pgproto3.Execute:
			r.kind = 'E'
			r.portal = s.portals[m.Portal]
			if r.portal != nil && r.portal.statement != nil {
				r.sql = r.portal.statement.sql
			}
		case *pgproto3.Close:
			r.kind = 'C'
			name := m.Name
			if m.ObjectType == 'S' {
				old, exists := s.statements[name]
				r.undo = func() {
					if exists {
						s.statements[name] = old
					}
				}
				delete(s.statements, name)
			} else {
				old, exists := s.portals[name]
				r.undo = func() {
					if exists {
						s.portals[name] = old
					}
				}
				delete(s.portals, name)
			}
		case *pgproto3.Sync:
			r.kind = 'S'
		case *pgproto3.Flush:
			r.kind = 'H'
		case *pgproto3.Terminate:
			s.mu.Unlock()
			return send(up, msg)
		default:
			s.mu.Unlock()
			return errors.New("unsupported PostgreSQL protocol message")
		}
		s.mu.Unlock()
		if r.kind == 'Q' || r.kind == 'E' || r.kind == 'P' {
			tokens, x := sqlTokens(r.sql)
			if x != nil {
				return x
			}
			for _, t := range tokens {
				if strings.EqualFold(t, "COPY") {
					_ = s.p.Engine.Record(ctx, "coverage_gap", recording.SandboxControl, map[string]any{"reason": "COPY protocol is not supported"}, nil)
					return errors.New("COPY is not supported by database capture")
				}
			}
		}
		if r.kind == 'Q' || r.kind == 'E' || r.kind == 'S' {
			observed, done := ctx, func() {}
			if s.p.Observe != nil {
				observed, done = s.p.Observe(ctx)
			}
			s.p.Engine.gate.RLock()
			r.ctx = observed
			timer := time.AfterFunc(time.Duration(s.p.Engine.Config.QueryTimeoutMS)*time.Millisecond, func() { s.timedOut.Store(true); up.Close() })
			r.done = func() { timer.Stop(); s.p.Engine.gate.RUnlock(); done() }
			if err = s.p.Engine.Check(ctx); err != nil {
				r.finish()
				return err
			}
			s.next++
			r.id = s.next
			s.mu.Lock()
			if r.kind != 'S' {
				saved := make(map[string]cursor, len(s.cursors))
				for k, v := range s.cursors {
					saved[k] = v
				}
				r.undo = func() { s.cursors = saved }
				r.sql = s.cursorQuery(r.sql, r)
			}
			s.mu.Unlock()
			raw, _ := msg.Encode(nil)
			detail := map[string]any{"connection": s.id, "execution": r.id, "query": r.sql}
			bound := r.portal
			if r.origin != nil {
				bound = r.origin
			}
			if bound != nil {
				detail["parameters"] = bound.params
				detail["parameter_formats"] = bound.formats
				if bound.statement != nil {
					detail["parameter_types"] = bound.statement.types
				}
			}
			if err = s.p.Engine.Record(r.ctx, "database_request", recording.Application, detail, raw); err != nil {
				r.finish()
				return err
			}
		}
		if r.kind == 'S' || r.kind == 'Q' {
			r.barrier = make(chan struct{})
		}
		if r.kind != 'H' {
			select {
			case q <- r:
			case <-ctx.Done():
				r.finish()
				return ctx.Err()
			}
		}
		if err = send(up, msg); err != nil {
			return err
		}
		// Keep each Sync group pipelined, but reconcile speculative metadata
		// before forwarding the next group after a native error.
		if r.barrier != nil {
			select {
			case <-r.barrier:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
func (s *connection) responses(ctx context.Context, f *pgproto3.Frontend, c net.Conn, q <-chan *execution) error {
	var r *execution
	var fields []pgproto3.FieldDescription
	var rows []*pgproto3.DataRow
	var buffered int64
	resultIndex := 0
	skip := false
	var undo []func()
	defer func() {
		if r != nil {
			r.finish()
		}
	}()
	for {
		m, err := f.Receive()
		if err != nil {
			return err
		}
		_ = c.SetWriteDeadline(time.Now().Add(time.Duration(s.p.Engine.Config.QueryTimeoutMS) * time.Millisecond))
		// Notices and asynchronous parameter changes do not consume command replies.
		switch m.(type) {
		case *pgproto3.NoticeResponse, *pgproto3.ParameterStatus, *pgproto3.NotificationResponse:
			if err = s.p.Engine.Record(ctx, "database_notice", recording.Application, m, nil); err != nil {
				return err
			}
			if err = send(c, m); err != nil {
				return err
			}
			continue
		}
		if r == nil {
			select {
			case r = <-q:
			case <-ctx.Done():
				return ctx.Err()
			}
			if r == nil {
				return errors.New("unexpected PostgreSQL reply")
			}
			fields = nil
			rows = nil
			buffered = 0
			resultIndex = 0
			if r.portal != nil && r.portal.statement != nil {
				s.mu.Lock()
				fields = cloneFields(r.portal.statement.columns)
				s.mu.Unlock()
				for i := range fields {
					formats := r.portal.resultFormats
					if len(formats) == 1 {
						fields[i].Format = formats[0]
					} else if i < len(formats) {
						fields[i].Format = formats[i]
					}
				}
			}
		}
		if skip {
			for r.kind != 'S' {
				if r.undo != nil {
					undo = append(undo, r.undo)
				}
				r.finish()
				select {
				case r = <-q:
				case <-ctx.Done():
					return ctx.Err()
				}
				if r == nil {
					return errors.New("pipeline ended without Sync")
				}
			}
			skip = false
		}
		complete := false
		switch v := m.(type) {
		case *pgproto3.RowDescription:
			fields = cloneFields(v.Fields)
			if r.kind == 'D' {
				s.mu.Lock()
				if r.statement != nil {
					r.statement.columns = fields
				}
				if r.portal != nil && r.portal.statement != nil {
					r.portal.statement.columns = fields
				}
				s.mu.Unlock()
				complete = true
			}
		case *pgproto3.ParameterDescription:
			s.mu.Lock()
			if r.statement != nil {
				r.statement.types = append([]uint32(nil), v.ParameterOIDs...)
			}
			s.mu.Unlock()
		case *pgproto3.DataRow:
			raw, _ := v.Encode(nil)
			buffered += int64(len(raw))
			if buffered > s.p.Engine.Config.MaxResultBytes {
				s.p.Engine.Recorder.Fail(errors.New("database result exceeds capture limit; connection closed with incomplete capture"))
				return s.p.Engine.Recorder.Err()
			}
			rows = append(rows, &pgproto3.DataRow{Values: cloneBytes(v.Values)})
			continue
		case *pgproto3.CommandComplete, *pgproto3.PortalSuspended, *pgproto3.EmptyQueryResponse:
			if r.kind == 'Q' || r.kind == 'E' {
				if err = s.releaseRows(r, c, fields, rows, resultIndex); err != nil {
					return err
				}
				rows = nil
				fields = nil
				buffered = 0
				resultIndex++
				if err = s.p.Engine.Record(r.ctx, "database_statement_outcome", recording.Application, map[string]any{"connection": s.id, "execution": r.id, "outcome": m}, nil); err != nil {
					return err
				}
			}
			complete = r.kind == 'E'
		case *pgproto3.ErrorResponse:
			if rows != nil {
				raw, _ := json.Marshal(rows)
				if err = s.p.Engine.Record(r.ctx, "database_partial_result", recording.Application, map[string]any{"connection": s.id, "execution": r.id, "columns": fields, "released": false}, raw); err != nil {
					return err
				}
			}
			rows = nil
			if r.undo != nil {
				undo = append(undo, r.undo)
				r.undo = nil
			}
			if err = s.p.Engine.Record(r.ctx, "database_error", recording.Application, map[string]any{"connection": s.id, "execution": r.id, "error": v}, nil); err != nil {
				return err
			}
			if r.kind != 'Q' {
				complete = true
				skip = true
			}
		case *pgproto3.ReadyForQuery:
			s.txStatus = v.TxStatus
			if v.TxStatus == 'I' {
				_ = c.SetReadDeadline(time.Time{})
			} else {
				_ = c.SetReadDeadline(time.Now().Add(time.Duration(s.p.Engine.Config.QueryTimeoutMS) * time.Millisecond))
			}
			s.mu.Lock()
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
			undo = nil
			s.mu.Unlock()
			if v.TxStatus == 'I' && s.p.After != nil {
				if err = s.p.After(r.ctx); err != nil {
					return err
				}
			}
			if err = s.p.Engine.Record(r.ctx, "database_transaction_status", recording.Application, map[string]any{"connection": s.id, "execution": r.id, "status": string([]byte{v.TxStatus})}, nil); err != nil {
				return err
			}
			complete = true
		case *pgproto3.ParseComplete, *pgproto3.BindComplete, *pgproto3.CloseComplete, *pgproto3.NoData:
			complete = true
		default:
			return fmt.Errorf("unsupported PostgreSQL backend reply %T", m)
		}
		if complete {
			r.finish()
		}
		if err = send(c, m); err != nil {
			return err
		}
		if complete {
			if r.barrier != nil {
				close(r.barrier)
			}
			r = nil
		}
	}
}
func (s *connection) releaseRows(r *execution, c net.Conn, fields []pgproto3.FieldDescription, rows []*pgproto3.DataRow, result int) error {
	if rows == nil {
		return nil
	}
	raw, _ := json.Marshal(rows)
	if err := s.p.Engine.Record(r.ctx, "database_result", recording.Application, map[string]any{"connection": s.id, "execution": r.id, "result": result, "columns": fields, "rows": len(rows)}, raw); err != nil {
		return err
	}
	values := make([]any, len(rows))
	for i, row := range rows {
		cells := make([]any, len(row.Values))
		for j, v := range row.Values {
			if j < len(fields) {
				cells[j] = decodeCell(fields[j], v)
			} else {
				cells[j] = map[string]any{"opaque": base64.StdEncoding.EncodeToString(v)}
			}
		}
		values[i] = cells
	}
	candidate := map[string]any{"rows": values}
	params := []any{}
	bound := r.portal
	if r.origin != nil {
		bound = r.origin
	}
	if bound != nil {
		for i, b := range bound.params {
			format := int16(0)
			if len(bound.formats) == 1 {
				format = bound.formats[0]
			} else if i < len(bound.formats) {
				format = bound.formats[i]
			}
			var oid uint32
			if bound.statement != nil && i < len(bound.statement.types) {
				oid = bound.statement.types[i]
			}
			var value any = string(b)
			if b == nil {
				value = nil
			} else if format == 1 {
				value = map[string]any{"binary": b}
			}
			params = append(params, map[string]any{"oid": oid, "format": format, "value": value})
		}
	}
	validate := func(value any) error {
		_, err := encodeRows(fields, rows, value, s.p.Engine.Config.MaxResultBytes)
		return err
	}
	changed, results, err := s.p.Injections.RewriteResponseChecked("database_response", s.p.Engine.ID, "", injection.ResponseContext{Body: map[string]any{"query": r.sql, "parameters": params, "result": result}}, candidate, validate)
	if err != nil {
		return err
	}
	if len(results) > 0 {
		rewritten, x := encodeRows(fields, rows, changed, s.p.Engine.Config.MaxResultBytes)
		if x != nil {
			return x
		}
		rows = rewritten
		after, _ := json.Marshal(rows)
		ids := []string{}
		for _, result := range results {
			if result.Applied {
				ids = append(ids, result.InjectionID)
			}
		}
		attemptID, payloadDigest := s.p.Injections.Attribution(ids)
		if err = s.p.Engine.Record(r.ctx, "database_response_rewritten", recording.HarnessInjected, map[string]any{"connection": s.id, "execution": r.id, "result": result, "injections": results, "attempt_id": attemptID, "payload_digest": payloadDigest}, after); err != nil {
			return err
		}
	}
	for _, row := range rows {
		if err = send(c, row); err != nil {
			return err
		}
	}
	return nil
}

func encodeRows(fields []pgproto3.FieldDescription, original []*pgproto3.DataRow, value any, limit int64) ([]*pgproto3.DataRow, error) {
	next, ok := value.(map[string]any)
	if !ok || len(next) != 1 {
		return nil, errors.New("database placement must preserve result shape")
	}
	values, ok := next["rows"].([]any)
	if !ok || len(values) != len(original) {
		return nil, errors.New("database placement must preserve row count")
	}
	rows := make([]*pgproto3.DataRow, len(original))
	var size int64
	for i, v := range values {
		cells, ok := v.([]any)
		if !ok || len(cells) != len(original[i].Values) {
			return nil, errors.New("database placement must preserve columns")
		}
		rows[i] = &pgproto3.DataRow{Values: make([][]byte, len(cells))}
		for j, value := range cells {
			if j >= len(fields) {
				return nil, errors.New("database result metadata unavailable")
			}
			b, err := encodeCell(fields[j], original[i].Values[j], value)
			if err != nil {
				return nil, err
			}
			rows[i].Values[j] = b
			size += int64(len(b)) + 4
			if size > limit {
				return nil, errors.New("injected database result exceeds limit")
			}
		}
	}
	return rows, nil
}
