package sandboxd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

const pythonAdapterFixture = `import asyncio, os
from helper import prefix
print('import diagnostic')
os.write(1, b'native diagnostic\n')
class Agent:
    def __init__(self):
        self.loop = asyncio.get_running_loop()
        self.count = 0
    async def invoke(self, value):
        assert asyncio.get_running_loop() is self.loop
        print('invoke diagnostic')
        if value.get('slow'): await asyncio.sleep(20)
        if value.get('error'): raise ValueError('deliberate failure')
        self.count += 1
        return {'count': self.count, 'answer': prefix + value['message']}
    async def close(self):
        assert asyncio.get_running_loop() is self.loop
        with open('closed', 'a') as f: f.write('closed\n')
async def initialize(options):
    print('initialize diagnostic')
    return Agent()
`
const nodeAdapterFixture = `import {prefix} from './helper.mjs';
import fs from 'node:fs';
console.log('import diagnostic'); fs.writeSync(1, 'native diagnostic\n');
export async function initialize(options) {
    console.log('initialize diagnostic');
    return {count: 0, async invoke(value) {
        console.log('invoke diagnostic');
        if(value.slow) await new Promise(resolve => setTimeout(resolve, 20000));
        if(value.error) throw new Error('deliberate failure');
        return {count: ++this.count, answer: prefix + value.message};
    }, async close() { fs.appendFileSync('closed', 'closed\n'); }};
}
`

func startCodeTest(t *testing.T, language, source string, concurrency int) (*protocolApplicationAdapter, string) {
	t.Helper()
	runtime, module, filename := "python3", "agent", "agent.py"
	if language == "node" {
		runtime, module, filename = "node", "./agent.mjs", "agent.mjs"
	}
	executable, err := exec.LookPath(runtime)
	if err != nil {
		t.Skipf("%s unavailable: %v", runtime, err)
	}
	root := t.TempDir()
	bundle := filepath.Join(root, environment.AdapterDirectory)
	if err := os.Mkdir(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	for file, text := range map[string]string{filename: source, "helper.py": "prefix = 'answer:'\n", "helper.mjs": "export const prefix = 'answer:';\n"} {
		if err := os.WriteFile(filepath.Join(bundle, file), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	config := &environment.CodeInterface{Language: language, Module: module, AdapterAsset: "assets/adapter", Initialize: "initialize", Invoke: "invoke", Close: "close", Concurrency: concurrency}
	argv, env, err := codeDriverCommand([]string{executable}, config)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{blueprint: &environment.Blueprint{Application: environment.Application{Interface: environment.ApplicationInterface{Adapter: "code", Code: config, Operations: []environment.ApplicationOperation{{ID: "ask"}}}}, Limits: environment.Limits{MaxBlobBytes: 1048576}}}
	a, err := newProtocolApplicationAdapter(s, argv, env)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), append(env, "INTERCEPTOR_PROTOCOL_FD=3")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	a.input, err = cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = []*os.File{writer}
	log, err := os.Create(filepath.Join(root, "diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	s.app = cmd
	done := make(chan struct{})
	go func() { defer close(done); defer reader.Close(); a.readFrames(reader, io.Discard) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = a.input.Close()
		_ = cmd.Wait()
		<-done
		log.Close()
	})
	return a, root
}

func TestInstalledCodeAdapters(t *testing.T) {
	for language, source := range map[string]string{"python": pythonAdapterFixture, "node": nodeAdapterFixture} {
		t.Run(language, func(t *testing.T) {
			a, root := startCodeTest(t, language, source, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := a.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= 2; n++ {
				result, err := a.Invoke(ctx, applicationInvocation{Operation: "ask", Body: []byte(`{"message":"hello"}`)})
				if err != nil {
					t.Fatal(err)
				}
				var value struct {
					Count  int
					Answer string
				}
				if err := json.Unmarshal(result.Body, &value); err != nil {
					t.Fatal(err)
				}
				if value.Count != n || value.Answer != "answer:hello" {
					t.Fatalf("state not retained: %s", result.Body)
				}
			}
			if _, err := a.Invoke(ctx, applicationInvocation{Operation: "ask", Body: []byte(`{"error":true}`)}); err == nil || !strings.Contains(err.Error(), "deliberate failure") {
				t.Fatalf("invocation error: %v", err)
			}
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			closed, err := os.ReadFile(filepath.Join(root, "closed"))
			if err != nil || string(closed) != "closed\n" {
				t.Fatalf("cleanup: %s %v", closed, err)
			}
			diagnostics, err := os.ReadFile(filepath.Join(root, "diagnostics"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(diagnostics), "initialize diagnostic") != 1 || !strings.Contains(string(diagnostics), "native diagnostic") {
				t.Fatalf("diagnostics missing: %s", diagnostics)
			}
		})
	}
}

func TestCodeCancellationInvalidatesSession(t *testing.T) {
	for language, source := range map[string]string{"python": pythonAdapterFixture, "node": nodeAdapterFixture} {
		t.Run(language, func(t *testing.T) {
			concurrency := 1
			if language == "node" {
				concurrency = 2
			}
			a, _ := startCodeTest(t, language, source, concurrency)
			ready, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := a.WaitReady(ready); err != nil {
				t.Fatal(err)
			}
			ctx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer stop()
			other := make(chan error, 1)
			if language == "node" {
				go func() {
					_, err := a.Invoke(ready, applicationInvocation{Operation: "ask", Body: []byte(`{"slow":true}`)})
					other <- err
				}()
			}
			if _, err := a.Invoke(ctx, applicationInvocation{Operation: "ask", Body: []byte(`{"slow":true}`)}); err == nil {
				t.Fatal("timed-out invocation succeeded")
			}
			if _, err := a.Invoke(ready, applicationInvocation{Operation: "ask", Body: []byte(`{"message":"again"}`)}); err == nil || !strings.Contains(err.Error(), "new session") {
				t.Fatalf("session reused: %v", err)
			}
			if language == "node" {
				select {
				case err := <-other:
					if err == nil {
						t.Fatal("concurrent call succeeded after cancellation")
					}
				case <-ready.Done():
					t.Fatal("concurrent call was left waiting")
				}
			}
		})
	}
}

func TestCodeStartupAndCleanupErrors(t *testing.T) {
	for language, source := range map[string]string{"python": pythonAdapterFixture, "node": nodeAdapterFixture} {
		for _, kind := range []string{"missing-invoke", "cleanup-error", "cleanup-timeout"} {
			t.Run(language+"/"+kind, func(t *testing.T) {
				changed := source
				switch kind {
				case "missing-invoke":
					changed = strings.ReplaceAll(changed, "def invoke(", "def other(")
					changed = strings.ReplaceAll(changed, "async invoke(", "async other(")
				case "cleanup-error":
					changed = strings.ReplaceAll(changed, "async def close(self):", "async def close(self):\n        raise ValueError('cleanup failed')")
					changed = strings.ReplaceAll(changed, "async close() {", "async close() { throw new Error('cleanup failed');")
				case "cleanup-timeout":
					changed = strings.ReplaceAll(changed, "async def close(self):", "async def close(self):\n        await asyncio.sleep(20)")
					changed = strings.ReplaceAll(changed, "async close() {", "async close() { await new Promise(resolve => setTimeout(resolve, 20000));")
				}
				a, _ := startCodeTest(t, language, changed, 1)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err := a.WaitReady(ctx)
				if kind == "missing-invoke" {
					if err == nil || !strings.Contains(err.Error(), "invocation methods") {
						t.Fatalf("startup: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := a.Close(ctx); err == nil {
					t.Fatal("cleanup failure not reported")
				}
			})
		}
	}
}
