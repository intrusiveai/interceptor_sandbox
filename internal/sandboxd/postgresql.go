package sandboxd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/appimage"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/postgresql"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func (s *Server) startDatabase(def environment.Service) error {
	if s.databases == nil {
		s.databases = map[string]*postgresql.Engine{}
		s.databaseProxies = map[string]*postgresql.Proxy{}
		s.databaseOracleBase = map[string]json.RawMessage{}
	}
	cert, err := tls.LoadX509KeyPair(appimage.ContainerTLSCertificate, appimage.ContainerTLSPrivateKey)
	if err != nil {
		return err
	}
	transactions, err := statejournal.VerifyFile(s.paths.Journal)
	if err != nil {
		return err
	}
	var restore *statejournal.Operation
	for _, tx := range transactions {
		for _, op := range tx.Operations {
			if op.Kind == "database_checkpoint" && op.Service == def.ID {
				copy := op
				restore = &copy
			}
		}
	}
	if s.metadata.ParentCheckpoint != "" && restore == nil {
		return fmt.Errorf("restored session lacks database checkpoint for %s", def.ID)
	}
	if restore != nil {
		var m postgresql.CheckpointMetadata
		if err := postgresql.ValidateCheckpoint(*restore); err != nil {
			return err
		}
		if err := json.Unmarshal(restore.Value, &m); err != nil {
			return err
		}
		for k, v := range m.OracleBaselines {
			s.databaseOracleBase[k] = v
		}
	}
	e := &postgresql.Engine{Failure: func(err error) {
		select {
		case s.done <- err:
		default:
		}
	}, ID: def.ID, Config: *def.PostgreSQL, Root: filepath.Join(s.paths.Root, "pg", def.ID), Recorder: s.recorder, Journal: s.journal, Blobs: s.blobs}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err = e.Start(ctx, containerEnvironment, restore); err != nil {
		return fmt.Errorf("start database %s: %w", def.ID, err)
	}
	s.databases[def.ID] = e
	if err = s.captureDatabaseOracles(ctx, def.ID, "baseline"); err != nil {
		return err
	}
	proxy := &postgresql.Proxy{Engine: e, TLS: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, Injections: s.injections, Observe: s.observeRequest, Failure: func(error) { _ = s.evidenceError() }, After: func(ctx context.Context) error { return s.captureDatabaseOracles(ctx, def.ID, "application") }}
	if err = proxy.Start(def.Listen); err != nil {
		return err
	}
	// The hostname participates in closed DNS and the existing TLS trust probe,
	// while ordinary HTTP requests have no database operation route.
	s.registerHostRoutes(def.Hostnames, http.NotFoundHandler())
	s.databaseProxies[def.ID] = proxy
	return nil
}
func (s *Server) captureDatabaseOracles(ctx context.Context, service, source string) error {
	s.databaseOracleMu.Lock()
	defer s.databaseOracleMu.Unlock()
	return s.captureDatabaseOraclesLocked(ctx, service, source)
}
func (s *Server) captureDatabaseOraclesLocked(ctx context.Context, service, source string) error {
	e := s.databases[service]
	if e == nil {
		return errors.New("database oracle service unavailable")
	}
	for _, o := range s.blueprint.Oracles {
		if o.Kind != "database_query_match" || o.Service != service {
			continue
		}
		sql, _ := o.Match["query"].(string)
		value, err := e.Oracle(ctx, sql)
		if err != nil {
			e.Recorder.Fail(fmt.Errorf("required database oracle %s capture failed: %w", o.ID, err))
			return e.Recorder.Err()
		}
		baseline, exists := s.databaseOracleBase[o.ID]
		if source == "harness" || !exists {
			s.databaseOracleBase[o.ID] = append(json.RawMessage{}, value...)
			baseline = value
		}
		expected, _ := json.Marshal(o.Match["equals"])
		eligible := source == "application" && !bytes.Equal(baseline, value) && bytes.Equal(expected, value)
		if err = e.Record(ctx, "database_oracle", recording.SandboxControl, map[string]any{"oracle": o.ID, "query": sql, "source": source, "result": value, "matched": eligible}, nil); err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) freezeDatabases(ctx context.Context) ([]statejournal.Operation, func(), error) {
	ids := []string{}
	for id := range s.databases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	releases := []func(){}
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, id := range ids {
		f, err := s.databases[id].Freeze(ctx)
		if err != nil {
			release()
			return nil, func() {}, err
		}
		releases = append(releases, f)
	}
	operations := []statejournal.Operation{}
	for _, id := range ids {
		op, err := s.databases[id].Backup(ctx)
		if err != nil {
			release()
			return nil, func() {}, err
		}
		s.databaseOracleMu.Lock()
		baselines := map[string]json.RawMessage{}
		for _, o := range s.blueprint.Oracles {
			if o.Service == id && o.Kind == "database_query_match" {
				baselines[o.ID] = s.databaseOracleBase[o.ID]
			}
		}
		op = postgresql.WithOracleBaselines(op, baselines)
		s.databaseOracleMu.Unlock()
		operations = append(operations, op)
	}
	return operations, release, nil
}
