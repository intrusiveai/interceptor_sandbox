package postgresql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/jackc/pgx/v5"
)

func (e *Engine) Table(id string) (Table, bool) {
	for _, t := range e.Config.Tables {
		if t.ID == id {
			return t, true
		}
	}
	return Table{}, false
}
func decodeObject(data []byte) (map[string]any, error) {
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	err := d.Decode(&v)
	if err == nil && v == nil {
		err = errors.New("record must be a JSON object")
	}
	return v, err
}
func (e *Engine) ValidateMutation(d injection.Definition) error {
	t, ok := e.Table(d.Selector.Collection)
	if !ok {
		return errors.New("database injection table is not declared")
	}
	key, err := decodeObject([]byte(d.Selector.Key))
	if err != nil || len(key) != len(t.Keys) {
		return errors.New("database key must be a JSON object with every declared unique key")
	}
	for _, name := range t.Keys {
		v, ok := key[name]
		if !ok || v == nil {
			return errors.New("database key cannot be absent or NULL")
		}
		switch v.(type) {
		case string, json.Number, bool:
		default:
			return errors.New("database key values must be scalar")
		}
	}
	if d.Placement.Pointer == "" && (d.Placement.Operation != "replace" || !t.AllowCreate) {
		return errors.New("database root replacement is reserved for creation-enabled tables")
	}
	return nil
}
func (e *Engine) Mutate(ctx context.Context, d injection.Definition) error {
	if err := e.ValidateMutation(d); err != nil {
		return err
	}
	e.gate.RLock()
	defer e.gate.RUnlock()
	e.adminMu.Lock()
	defer e.adminMu.Unlock()
	if err := e.Check(ctx); err != nil {
		return err
	}
	t, _ := e.Table(d.Selector.Collection)
	key, _ := decodeObject([]byte(d.Selector.Key))
	table := pgx.Identifier{t.Schema, t.Name}.Sanitize()
	tx, err := e.admin.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	// Serialize authorized mutations on this table, including count-and-create.
	if _, err = tx.Exec(ctx, "LOCK TABLE "+table+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return err
	}
	predicates := []string{}
	args := []any{}
	for _, name := range t.Keys {
		args = append(args, fmt.Sprint(key[name]))
		predicates = append(predicates, pgx.Identifier{name}.Sanitize()+fmt.Sprintf("=$%d", len(args)))
	}
	where := strings.Join(predicates, " AND ")
	rows, err := tx.Query(ctx, "SELECT to_jsonb(t) FROM "+table+" AS t WHERE "+where+" FOR UPDATE", args...)
	if err != nil {
		return err
	}
	var current json.RawMessage
	count := 0
	for rows.Next() {
		count++
		if err = rows.Scan(&current); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if count > 1 {
		return errors.New("database injection key is not unique")
	}
	allowed := map[string]bool{}
	for _, name := range t.Fields {
		allowed[name] = true
	}
	create := count == 0
	var after map[string]any
	if create {
		if !t.AllowCreate || d.Placement.Pointer != "" || d.Placement.Operation != "replace" {
			return errors.New("database record is absent; creation requires explicit policy and root replace")
		}
		after, err = decodeObject(d.Payload)
		if err != nil {
			return err
		}
		for _, name := range t.Keys {
			if !reflect.DeepEqual(key[name], after[name]) {
				return errors.New("created record keys must equal selector key")
			}
			allowed[name] = true
		}
		for name := range after {
			if !allowed[name] {
				return fmt.Errorf("database creation field %s is not allowed", name)
			}
		}
		var n int
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			return err
		}
		if n >= t.MaxRecords {
			return errors.New("database creation record limit reached")
		}
	} else {
		if d.Placement.Pointer == "" {
			return errors.New("database record already exists; creation never upserts")
		}
		before, x := decodeObject(current)
		if x != nil {
			return x
		}
		v, x := injection.Apply(before, d.Placement, d.Payload)
		if x != nil {
			return x
		}
		var ok bool
		after, ok = v.(map[string]any)
		if !ok {
			return errors.New("database update must preserve record shape")
		}
		original, _ := decodeObject(current)
		for name, value := range after {
			if !reflect.DeepEqual(value, original[name]) && !allowed[name] {
				return fmt.Errorf("database field %s is not writable by injection", name)
			}
		}
		for name := range original {
			if _, ok := after[name]; !ok {
				return errors.New("database injection cannot delete fields")
			}
		}
	}
	afterBytes, _ := json.Marshal(after)
	if err = e.Record(ctx, "database_state_injection_requested", recording.HarnessInjected, map[string]any{"injection_id": d.ID, "attempt_id": d.AttemptID, "payload_digest": d.PayloadDigest, "table": t.ID, "key": key, "create": create, "before": current, "after": json.RawMessage(afterBytes)}, nil); err != nil {
		return err
	}
	names := []string{}
	for name := range after {
		if allowed[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	columns := []string{}
	for _, name := range names {
		columns = append(columns, pgx.Identifier{name}.Sanitize())
	}
	// jsonb_populate_record performs native typed conversion and preserves all
	// constraints. Identifiers come exclusively from sealed table policy.
	if create {
		sql := "INSERT INTO " + table + " (" + strings.Join(columns, ",") + ") SELECT " + strings.Join(columns, ",") + " FROM jsonb_populate_record(NULL::" + table + ",$1::jsonb)"
		_, err = tx.Exec(ctx, sql, string(afterBytes))
	} else {
		assigns := []string{}
		for _, name := range columns {
			assigns = append(assigns, name+"=v."+name)
		}
		updateArgs := append([]any{string(afterBytes)}, args...)
		predicates = nil
		for i, name := range t.Keys {
			predicates = append(predicates, "t."+pgx.Identifier{name}.Sanitize()+fmt.Sprintf("=$%d", i+2))
		}
		_, err = tx.Exec(ctx, "UPDATE "+table+" AS t SET "+strings.Join(assigns, ",")+" FROM jsonb_populate_record(NULL::"+table+",$1::jsonb) AS v WHERE "+strings.Join(predicates, " AND "), updateArgs...)
	}
	if err != nil {
		return err
	}
	var committedRow json.RawMessage
	if err = tx.QueryRow(ctx, "SELECT to_jsonb(t) FROM "+table+" AS t WHERE "+where, args...).Scan(&committedRow); err != nil {
		return err
	}
	if err = e.Recorder.Err(); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		e.Recorder.Fail(fmt.Errorf("database injection commit outcome is uncertain: %w", err))
		return e.Recorder.Err()
	}
	return e.Record(ctx, "database_state_injection_committed", recording.HarnessInjected, map[string]any{"injection_id": d.ID, "attempt_id": d.AttemptID, "payload_digest": d.PayloadDigest, "table": t.ID, "key": key, "create": create, "after": committedRow}, nil)
}
