package sandboxd

import (
	"context"
	"encoding/json"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const pythonStateFixture = `import asyncio
class Agent:
    def __init__(self):
        self.count = 0
        self.mode = ''
        self.loop = asyncio.get_running_loop()
    async def invoke(self, value):
        self.mode = value.get('mode', '')
        if value.get('slow'): await asyncio.sleep(.2)
        self.count += 1
        return {'count': self.count}
    async def save_state(self):
        assert self.loop is asyncio.get_running_loop()
        if self.mode == 'slow': await asyncio.sleep(20)
        if self.mode == 'error': raise ValueError('save rejected')
        if self.mode == 'nan': return {'bad': float('nan')}
        if self.mode == 'key': return {1: 'lost'}
        if self.mode == 'object': return {'bad': object()}
        if self.mode == 'big': return {'bad': 'x'*100000}
        if self.mode == 'cycle':
            v = {}; v['v'] = v; return v
        return {'count': self.count, 'identity': 'alice', 'messages': [{'role':'user','content':'hello'}]}
    async def restore_state(self, value):
        assert self.loop is asyncio.get_running_loop()
        if value.get('identity') != 'alice': raise ValueError('incompatible identity')
        self.count = value['count']
    def close(self): pass
def initialize(options): return Agent()
`
const nodeStateFixture = `export function initialize() { return {
 count: 0, mode: '',
 async invoke(value) { this.mode = value.mode || ''; if(value.slow) await new Promise(r=>setTimeout(r,200)); return {count: ++this.count}; },
 async save_state() {
  if(this.mode === 'slow') await new Promise(r=>setTimeout(r,20000));
  if(this.mode === 'error') throw new Error('save rejected');
  if(this.mode === 'nan') return {bad: NaN};
  if(this.mode === 'key') return {bad: undefined};
  if(this.mode === 'object') return {bad: new Date()};
  if(this.mode === 'big') return {bad: 'x'.repeat(100000)};
  if(this.mode === 'cycle') { const v={}; v.v=v; return v; }
  return {count: this.count, identity: 'alice', messages: [{role:'user',content:'hello'}]};
 },
 async restore_state(value) {if(value.identity !== 'alice') throw new Error('incompatible identity'); this.count=value.count;},
 close() {}
}; }
`

func stateTest(t *testing.T, language, source, filename string, restore bool) *protocolApplicationAdapter {
	t.Helper()
	a, _ := startCodeTestConfigured(t, language, source, 1, func(root string, c *environment.CodeInterface) []string {
		c.State = &environment.CodeState{Save: "save_state", Restore: "restore_state", Path: filename}
		mode := "false"
		if restore {
			mode = "true"
		}
		return []string{"INTERCEPTOR_STATE_RESTORE=" + mode, "INTERCEPTOR_STATE_MAX_BYTES=4096"}
	})
	return a
}
func stateFilename(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "state.json")
}
func readyState(t *testing.T, a *protocolApplicationAdapter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
}
func stateInvoke(t *testing.T, a *protocolApplicationAdapter, input string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := a.Invoke(ctx, applicationInvocation{Operation: "ask", Body: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}
	return string(r.Body)
}
func saveState(t *testing.T, a *protocolApplicationAdapter) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := a.lockState(ctx)
	if err != nil {
		return err
	}
	defer release()
	return a.saveState(ctx)
}

func TestStateRoundTripAndRejection(t *testing.T) {
	for language, source := range map[string]string{"python": pythonStateFixture, "node": nodeStateFixture} {
		for _, synchronous := range []bool{false, true} {
			name := language
			if synchronous {
				name += "-sync"
			}
			t.Run(name, func(t *testing.T) {
				if synchronous {
					if language == "python" {
						source = strings.ReplaceAll(source, "async def save_state", "def save_state")
						source = strings.ReplaceAll(source, "async def restore_state", "def restore_state")
						source = strings.ReplaceAll(source, "if self.mode == 'slow': await asyncio.sleep(20)", "if self.mode == 'slow': raise ValueError('slow disabled')")
					} else {
						source = strings.ReplaceAll(source, "async save_state", "save_state")
						source = strings.ReplaceAll(source, "async restore_state", "restore_state")
						source = strings.ReplaceAll(source, "if(this.mode === 'slow') await new Promise(r=>setTimeout(r,20000));", "")
					}
				}
				filename := stateFilename(t)
				a := stateTest(t, language, source, filename, false)
				readyState(t, a)
				// An empty conversation is an explicit valid checkpoint.
				if err := saveState(t, a); err != nil {
					t.Fatal(err)
				}
				stateInvoke(t, a, `{}`)
				stateInvoke(t, a, `{}`)
				if err := saveState(t, a); err != nil {
					t.Fatal(err)
				}
				baseline, _ := os.ReadFile(filename)
				for _, mode := range []string{"error", "nan", "key", "object", "big", "cycle"} {
					stateInvoke(t, a, `{"mode":"`+mode+`"}`)
					if err := saveState(t, a); err == nil {
						t.Fatalf("accepted %s", mode)
					}
					after, _ := os.ReadFile(filename)
					if string(after) != string(baseline) {
						t.Fatalf("%s replaced valid state", mode)
					}
				}
				for n := 0; n < 2; n++ {
					b := stateTest(t, language, source, filename, true)
					readyState(t, b)
					var result struct{ Count int }
					json.Unmarshal([]byte(stateInvoke(t, b, `{}`)), &result)
					if result.Count != 3+n {
						t.Fatalf("restore lost history: %+v", result)
					}
					stateInvoke(t, b, `{}`) // later turns retain the loaded state
					if n == 0 {             // select an earlier point, not the current in-memory history
						os.WriteFile(filename, baseline, 0600)
						c := stateTest(t, language, source, filename, true)
						readyState(t, c)
						stateInvoke(t, c, `{}`)
						if err := saveState(t, c); err != nil {
							t.Fatal(err)
						}
					}
				}
				fresh := stateTest(t, language, source, filename, false)
				readyState(t, fresh)
				var result struct{ Count int }
				json.Unmarshal([]byte(stateInvoke(t, fresh, `{}`)), &result)
				if result.Count != 1 {
					t.Fatal("fresh start loaded stale checkpoint")
				}
			})
		}
	}
}
func TestStateRestoreFailures(t *testing.T) {
	for language, source := range map[string]string{"python": pythonStateFixture, "node": nodeStateFixture} {
		for _, kind := range []string{"missing", "corrupt", "version", "identity", "oversized", "symlink", "parent-link", "missing-method"} {
			t.Run(language+"/"+kind, func(t *testing.T) {
				filename := stateFilename(t)
				data := `{"version":1,"state":{"identity":"alice","count":0}}`
				switch kind {
				case "corrupt":
					data = "{"
				case "version":
					data = `{"version":2,"state":{}}`
				case "identity":
					data = `{"version":1,"state":{}}`
				case "oversized":
					data = strings.Repeat("x", 5000)
				case "missing-method":
					source = strings.ReplaceAll(source, "save_state", "other")
				}
				if kind != "missing" {
					if err := os.WriteFile(filename, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "symlink" {
					os.Rename(filename, filename+".real")
					os.Symlink(filename+".real", filename)
				}
				if kind == "parent-link" {
					os.Symlink(filepath.Dir(filename), filename+".link")
					filename = filepath.Join(filename+".link", "state.json")
				}
				a := stateTest(t, language, source, filename, true)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := a.WaitReady(ctx); err == nil {
					t.Fatal("invalid restore announced readiness")
				}
			})
		}
	}
}
func TestStateGateAndTimeout(t *testing.T) {
	for language, source := range map[string]string{"python": pythonStateFixture, "node": nodeStateFixture} {
		t.Run(language, func(t *testing.T) {
			a := stateTest(t, language, source, stateFilename(t), false)
			readyState(t, a)
			release, err := a.lockState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			if _, err := a.Invoke(ctx, applicationInvocation{Operation: "ask", Body: []byte(`{}`)}); err == nil {
				t.Fatal("invocation passed snapshot gate")
			}
			cancel()
			release()
			stateInvoke(t, a, `{"mode":"slow"}`)
			if err := saveState(t, a); err == nil {
				t.Fatal("slow export succeeded")
			}
			if _, err := a.Invoke(context.Background(), applicationInvocation{Operation: "ask"}); err == nil {
				t.Fatal("uncertain state session reused")
			}
		})
	}
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := lockContext(ctx, mu.TryLock); err == nil {
		t.Fatal("lock ignored deadline")
	}
}

func TestStateSnapshotCoordinatesDirectAndControlInvocations(t *testing.T) {
	for _, route := range []string{"direct", "control"} {
		t.Run(route, func(t *testing.T) {
			filename := stateFilename(t)
			s, control := nativeOperationServer(t)
			service := httptest.NewServer(s.trackActivity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })))
			defer service.Close()
			source := strings.Replace(pythonStateFixture, "if value.get('slow'): await asyncio.sleep(.2)", "if value.get('slow'):\n            await asyncio.sleep(.1)\n            from urllib.request import urlopen\n            urlopen('"+service.URL+"').read()\n            await asyncio.sleep(.1)", 1)
			a := stateTest(t, "python", source, filename, false)
			readyState(t, a)
			s.blueprint.Application = a.server.blueprint.Application
			s.blueprint.Application.Interface.TurnTimeoutMS = 1000
			s.blueprint.Application.Persistence = environment.Persistence{SnapshotCapable: true, MutableRoots: []string{filepath.Dir(filename)}, Quiesce: environment.QuiesceConfig{Kind: "none", TimeoutMS: 2000}}
			s.app = a.server.app
			s.adapter = a
			a.server = s
			s.metadata.EnvironmentDigest = digest.Bytes([]byte("env"))
			s.metadata.AppDigest = digest.Bytes([]byte("app"))
			if err := snapshot.WriteManifest(s.paths.FileManifest, snapshot.EmptyManifest()); err != nil {
				t.Fatal(err)
			}
			if _, _, _, ok := s.resolveInjectionFile(injection.Selector{Path: filename}); ok {
				t.Fatal("checkpoint exposed to direct file injection")
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				url, body, handler := "/operations/ask", `{"slow":true}`, a.Handler()
				if route == "control" {
					url = "/v1/turns"
					body = `{"operation":"ask","input":"eyJzbG93Ijp0cnVlfQ==","media_type":"application/json"}`
					handler = control
				}
				w := httptest.NewRecorder()
				r := httptest.NewRequest("POST", url, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				handler.ServeHTTP(w, r)
				done <- w
			}()
			deadline := time.Now().Add(time.Second)
			for len(a.stateGate) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if len(a.stateGate) == 0 {
				t.Fatalf("invocation did not enter gate: %s", (<-done).Body.String())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cp, err := s.createSnapshot(ctx, "with-context", snapshot.ByteAllowance{}, snapshot.Metadata{})
			if err != nil {
				t.Fatal(err)
			}
			w := <-done
			if w.Code >= 300 {
				t.Fatalf("turn: %d %s", w.Code, w.Body)
			}
			data, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct{ State struct{ Count int } }
			json.Unmarshal(data, &saved)
			if saved.State.Count != 1 || cp.CanonicalSizeBytes < int64(len(data)) {
				t.Fatalf("snapshot missed completed turn: %s %+v", data, cp)
			}
			stateInvoke(t, a, `{"mode":"error"}`)
			if _, err := s.createSnapshot(ctx, "rejected", snapshot.ByteAllowance{}, snapshot.Metadata{}); err == nil {
				t.Fatal("failed export used stale state")
			}
			checkpoints, err := snapshot.ListCheckpoints(s.paths.Checkpoints)
			if err != nil || len(checkpoints) != 1 {
				t.Fatalf("failed save published: %v %v", checkpoints, err)
			}
			stateInvoke(t, a, `{}`)
			if _, err := s.createSnapshot(ctx, "too-small", snapshot.ByteAllowance{Value: 1, Set: true}, snapshot.Metadata{}); err == nil {
				t.Fatal("state bypassed snapshot budget")
			}
			stateInvoke(t, a, `{}`)
		})
	}
}
