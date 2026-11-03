package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/services"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

type evidenceConnector func(context.Context, bridge.ModelFrame) (bridge.ModelResult, error)

func (f evidenceConnector) Forward(ctx context.Context, frame bridge.ModelFrame) (bridge.ModelResult, error) {
	return f(ctx, frame)
}

func evidenceProxy(t *testing.T, s *Server, forward evidenceConnector) http.Handler {
	t.Helper()
	proxy, err := bridge.NewModelProxy(s.metadata.ID, forward, nil, s.blobs, s.recorder, 1<<20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return s.observedHandler(proxy, "")
}
func modelRequest() *http.Request {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture","messages":[{"role":"user","content":"hello"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Interceptor-Turn-ID", "forged")
	return r
}
func modelSuccess(_ context.Context, frame bridge.ModelFrame) (bridge.ModelResult, error) {
	return bridge.ModelResult{Status: 200, Body: []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`), FinalRequest: frame.Body}, nil
}
func allEvidence(t *testing.T, s *Server) []recording.Event {
	t.Helper()
	events, err := recording.VerifyFile(s.paths.Events)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestObservedWindowsAreImmutableAndNeverTrustHeaders(t *testing.T) {
	s, _ := nativeOperationServer(t)
	proxy := evidenceProxy(t, s, modelSuccess)
	observe := func() {
		t.Helper()
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, modelRequest())
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	observe() // startup/between turns
	a, err := s.beginInvocation(context.Background(), applicationInvocation{TurnID: "a", AttemptID: "attempt-a"})
	if err != nil {
		t.Fatal(err)
	}
	observe()
	b, err := s.beginInvocation(context.Background(), applicationInvocation{TurnID: "b"})
	if err != nil {
		t.Fatal(err)
	}
	observe()
	b()
	observe()
	a()
	observe()
	expected := []string{"", "a", "", "a", ""}
	windows := []string{"outside_turn", "observed", "ambiguous", "observed", "outside_turn"}
	index := 0
	for _, e := range allEvidence(t, s) {
		if e.Kind != "model_exchange" {
			continue
		}
		if e.TurnID != expected[index] || e.Attribution.Window != windows[index] {
			t.Fatalf("event %d: %+v", index, e)
		}
		if e.TurnID == "a" && (e.Attribution.ObservedAttemptID != "attempt-a" || e.AttemptID != "") {
			t.Fatalf("observation confused with causal origin: %+v", e)
		}
		index++
	}
	if index != len(expected) {
		t.Fatal(index)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	slow := evidenceProxy(t, s, func(ctx context.Context, f bridge.ModelFrame) (bridge.ModelResult, error) {
		close(entered)
		<-release
		return modelSuccess(ctx, f)
	})
	a, _ = s.beginInvocation(context.Background(), applicationInvocation{TurnID: "late"})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); slow.ServeHTTP(w, modelRequest()); done <- w }()
	<-entered
	a()
	if !s.pendingEvidence("late") {
		t.Fatal("lost pending request")
	}
	b, _ = s.beginInvocation(context.Background(), applicationInvocation{TurnID: "next"})
	close(release)
	w := <-done
	b()
	if w.Code != 200 || s.pendingEvidence("late") {
		t.Fatal(w.Code, "pending request not closed")
	}
	events := allEvidence(t, s)
	last := events[len(events)-1]
	if last.Kind != "model_exchange" || last.TurnID != "late" {
		t.Fatalf("late response reassigned: %+v", last)
	}
}

func TestHTTPInvocationObservesModelAndServiceWithoutHeader(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprint(direct), func(t *testing.T) {
			s, _ := nativeOperationServer(t)
			model := httptest.NewServer(evidenceProxy(t, s, modelSuccess))
			defer model.Close()
			fixture := services.NewFixtureHandler("tickets", &services.FixtureSet{Rules: []services.FixtureRule{{ID: "read", Match: services.FixtureMatch{Method: "GET", Path: "/tickets"}, Response: services.FixtureResponse{Status: 200, Body: "ok"}}}}, t.TempDir(), s.recorder)
			service := httptest.NewServer(s.observedHandler(fixture, "tickets"))
			defer service.Close()
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Interceptor-Turn-ID") != "" {
					t.Error("Interceptor still sends turn header")
				}
				for i := 0; i < 2; i++ {
					response, err := http.Post(model.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fixture","messages":[{"role":"user","content":"hello"}]}`))
					if err != nil {
						t.Error(err)
						return
					}
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
				response, err := http.Get(service.URL + "/tickets")
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				w.Write([]byte("done"))
			}))
			defer target.Close()
			u, _ := url.Parse(target.URL)
			s.blueprint.Application.Interface.Adapter = "http"
			s.blueprint.Application.Interface.Operations = []environment.ApplicationOperation{{ID: "invoke", Method: "POST", Path: "/ask"}}
			a := &httpApplicationAdapter{server: s, target: u, proxy: httputil.NewSingleHostReverseProxy(u)}
			s.adapter = a
			w := httptest.NewRecorder()
			if direct {
				a.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/ask", strings.NewReader("hello")))
			} else {
				s.turnHandler(w, httptest.NewRequest("POST", "/v1/turns", strings.NewReader(`{"operation":"invoke","input":"aGVsbG8="}`)))
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			count := 0
			var id string
			for _, e := range allEvidence(t, s) {
				if e.Kind == "model_exchange" || e.Kind == "fixture_hit" {
					if e.TurnID == "" || e.Attribution.Window != "observed" {
						t.Fatalf("missing observation: %+v", e)
					}
					if id != "" && id != e.TurnID {
						t.Fatal("different windows")
					}
					id = e.TurnID
					count++
				}
			}
			if count != 3 {
				t.Fatal(count)
			}
		})
	}
}

func TestEvidenceFailurePreventsWorkAndMarksSession(t *testing.T) {
	for _, stage := range []string{"request_blob", "response_blob", "request_event", "response_event", "turn_output", "journal"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := nativeOperationServer(t)
			calls := 0
			if stage == "request_blob" {
				if err := os.Rename(s.paths.Blobs, s.paths.Blobs+"-unavailable"); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "request_event" {
				s.recorder.Close()
			}
			proxy := evidenceProxy(t, s, func(ctx context.Context, f bridge.ModelFrame) (bridge.ModelResult, error) {
				calls++
				if stage == "response_blob" {
					if err := os.Rename(s.paths.Blobs, s.paths.Blobs+"-unavailable"); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "response_event" {
					s.recorder.Close()
				}
				return modelSuccess(ctx, f)
			})
			w := httptest.NewRecorder()
			if stage == "turn_output" {
				os.Rename(s.paths.Blobs, s.paths.Blobs+"-unavailable")
				s.turnHandler(w, httptest.NewRequest("POST", "/v1/turns", strings.NewReader(`{"operation":"invoke","input":"eA=="}`)))
				var turn Turn
				json.Unmarshal(w.Body.Bytes(), &turn)
				if turn.Status != "error" || !turn.EvidencePartial {
					t.Fatal(w.Body)
				}
			} else if stage == "journal" {
				s.journal.SetFailureHandler(s.recorder.Fail)
				s.journal.Close()
				seed, routes := filepath.Join(t.TempDir(), "seed.json"), filepath.Join(t.TempDir(), "routes.yaml")
				os.WriteFile(seed, []byte(`{"items":{"one":{"id":"one"}}}`), 0600)
				os.WriteFile(routes, []byte("routes:\n  - id: delete\n    method: DELETE\n    path: /items/{id}\n    action: delete\n    collection: items\n"), 0600)
				h, err := services.LoadStateful(seed, routes, "items", s.journal, s.recorder)
				if err != nil {
					t.Fatal(err)
				}
				s.observedHandler(h, "items").ServeHTTP(w, httptest.NewRequest("DELETE", "/items/one", nil))
				if w.Code != 507 {
					t.Fatal(w.Code, w.Body)
				}
			} else {
				proxy.ServeHTTP(w, modelRequest())
				if w.Code != 507 {
					t.Fatal(w.Code, w.Body)
				}
				expected := 1
				if strings.HasPrefix(stage, "request") {
					expected = 0
				}
				if calls != expected {
					t.Fatal("provider calls", calls, "want", expected)
				}
			}
			if s.evidenceError() == nil {
				t.Fatal("failure not latched")
			}
			if _, err := s.beginInvocation(context.Background(), applicationInvocation{TurnID: "blocked"}); err == nil {
				t.Fatal("new invocation admitted")
			}
			if _, err := s.evaluate(); err == nil {
				t.Fatal("evaluation reported success")
			}
			if _, err := s.createSnapshot(context.Background(), "bad", snapshot.ByteAllowance{}, snapshot.Metadata{}); err == nil {
				t.Fatal("snapshot admitted")
			}
			if s.metadata.Phase != "error" || len(s.metadata.Taints) == 0 {
				t.Fatal(s.metadata)
			}
			before := s.recorder.Err().Error()
			s.recorder.Fail(errors.New("later failure"))
			if before != s.recorder.Err().Error() {
				t.Fatal("first failure replaced")
			}
		})
	}
}

func TestQueuedInvocationAndCancelledExecution(t *testing.T) {
	s, _ := nativeOperationServer(t)
	s.blueprint.Application.Interface.Command = &environment.CommandInterface{}
	a := &commandApplicationAdapter{server: s, limit: make(chan struct{}, 1)}
	a.limit <- struct{}{}
	done := make(chan error, 1)
	go func() {
		_, err := a.Invoke(context.Background(), applicationInvocation{TurnID: "queued", Operation: "invoke"})
		done <- err
	}()
	ctx, finish := s.observeRequest(context.Background())
	finish()
	if recording.ObservationFrom(ctx).TurnID != "" {
		t.Fatal("queued work became active")
	}
	s.recorder.Fail(errors.New("disk full"))
	<-a.limit
	if err := <-done; err == nil || !strings.Contains(err.Error(), "evidence_storage_error") {
		t.Fatal(err)
	}
	s2, _ := nativeOperationServer(t)
	c, cancel := context.WithCancel(context.Background())
	end, _ := s2.beginInvocation(c, applicationInvocation{TurnID: "cancelled"})
	cancel()
	end()
	end, _ = s2.beginInvocation(context.Background(), applicationInvocation{TurnID: "next"})
	defer end()
	ctx, finish = s2.observeRequest(context.Background())
	defer finish()
	if o := recording.ObservationFrom(ctx); o.TurnID != "" || o.Window != "uncertain_execution" {
		t.Fatal(o)
	}
}

func TestFailureStatusPersistenceErrorIsVisible(t *testing.T) {
	s, _ := nativeOperationServer(t)
	s.paths.Metadata = filepath.Join(t.TempDir(), "directory")
	os.Mkdir(s.paths.Metadata, 0700)
	s.recorder.Fail(errors.New("disk full"))
	w := httptest.NewRecorder()
	s.statusHandler(w, httptest.NewRequest("GET", "/v1/status", nil))
	if !strings.Contains(w.Body.String(), "could not be persisted") {
		t.Fatal(w.Body)
	}
}

// Storage failures must win even when the application catches the service error.
func TestApplicationCannotHideEvidenceFailure(t *testing.T) {
	s, _ := nativeOperationServer(t)
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		s.recorder.Fail(errors.New("required model response lost"))
		w.Write([]byte("success"))
	}))
	defer target.Close()
	u, _ := url.Parse(target.URL)
	s.adapter = &httpApplicationAdapter{server: s, target: u}
	s.blueprint.Application.Interface.Operations = []environment.ApplicationOperation{{ID: "invoke", Method: "POST", Path: "/ask"}}
	w := httptest.NewRecorder()
	s.turnHandler(w, httptest.NewRequest("POST", "/v1/turns", strings.NewReader(`{"operation":"invoke"}`)))
	var turn Turn
	json.Unmarshal(w.Body.Bytes(), &turn)
	if turn.Status != "error" || !strings.Contains(turn.Error, "evidence_storage_error") {
		t.Fatal(w.Body)
	}
	w = httptest.NewRecorder()
	s.turnHandler(w, httptest.NewRequest("POST", "/v1/turns", strings.NewReader(`{"operation":"invoke"}`)))
	if w.Code != 507 || calls.Load() != 1 {
		t.Fatal(w.Code, calls.Load())
	}
}

func TestDirectApplicationErrorCannotHideCaptureFailure(t *testing.T) {
	s, _ := nativeOperationServer(t)
	handler := s.adapterHandler(func(context.Context, applicationInvocation) (applicationResult, error) {
		s.recorder.Fail(errors.New("response capture failed"))
		return applicationResult{}, errors.New("application caught HTTP 507")
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/operations/invoke", strings.NewReader("hello")))
	if w.Code != 507 || !strings.Contains(w.Body.String(), `"code":"evidence_storage_error"`) {
		t.Fatal(w.Code, w.Body)
	}
}
