package sandboxd

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

func TestBindOperationArguments(t *testing.T) {
	bindings := []environment.ArgumentBinding{
		{Literal: "--prompt"},
		{From: "/request/prompt"},
		{Literal: "--count"},
		{From: "/request/count"},
	}
	arguments, err := bindOperationArguments(bindings, []byte(`{"request":{"prompt":"hello","count":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	wanted := []string{"--prompt", "hello", "--count", "2"}
	if stringValue, _ := json.Marshal(arguments); string(stringValue) != `["--prompt","hello","--count","2"]` {
		t.Fatalf("got %v, want %v", arguments, wanted)
	}
}

func TestHelperChild(t *testing.T) {
	if os.Getenv("INTERCEPTOR_TEST_HELPER") != "1" {
		return
	}
	if err := RunHelper(); err != nil {
		t.Fatal(err)
	}
}

func TestHelperCorrelatesConcurrentResultsAndCancelsOneCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cancellation is Unix-specific")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestHelperChild$")
	command.Env = append(os.Environ(), "INTERCEPTOR_TEST_HELPER=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	encoder := json.NewEncoder(stdin)
	for _, request := range []HelperRequest{
		{ID: "slow", Type: "invoke", Command: []string{"/bin/sh", "-c", "sleep 10"}, TimeoutMS: 15000},
		{ID: "fast", Type: "invoke", Command: []string{"/bin/sh", "-c", "printf fast"}, TimeoutMS: 5000},
		{ID: "slow", Type: "cancel"},
	} {
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
	}
	responses := make(chan HelperResponse, 2)
	go func() {
		scanner := bufio.NewScanner(stdout)
		count := 0
		for scanner.Scan() {
			var response HelperResponse
			if json.Unmarshal(scanner.Bytes(), &response) == nil {
				responses <- response
				count++
			}
			if count == 2 {
				return
			}
		}
	}()
	result := map[string]HelperResponse{}
	for len(result) < 2 {
		select {
		case response := <-responses:
			result[response.ID] = response
		case <-time.After(3 * time.Second):
			t.Fatal("helper did not return both correlated responses after cancellation")
		}
	}
	if !result["fast"].OK || string(result["fast"].Stdout) != "fast" {
		t.Fatalf("unexpected fast response: %+v", result["fast"])
	}
	if result["slow"].OK || result["slow"].Error == "" {
		t.Fatalf("canceled command reported success: %+v", result["slow"])
	}
}

func TestCodeDrivers(t *testing.T) {
	tests := []struct {
		name, language, runtime, module, filename, source string
	}{
		{
			name: "python", language: "python", runtime: "python3", module: "agent", filename: "agent.py",
			source: "class Agent:\n    def __init__(self, options): self.prefix=options['prefix']\n    async def invoke(self, value): return {'answer':self.prefix+value['prompt']}\ndef create(options): return Agent(options)\n",
		},
		{
			name: "node", language: "node", runtime: "node", module: "./agent.mjs", filename: "agent.mjs",
			source: "export async function create(options) { return {prefix: options.prefix, async invoke(value) { return {answer:this.prefix+value.prompt}; }}; }\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, err := exec.LookPath(test.runtime)
			if err != nil {
				t.Skipf("%s runtime is unavailable", test.language)
			}
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, test.filename), []byte(test.source), 0o600); err != nil {
				t.Fatal(err)
			}
			config := &environment.CodeInterface{Language: test.language, Module: test.module, Initialize: "create", Invoke: "invoke", InputMode: "single", InitArguments: map[string]any{"prefix": "result:"}}
			argv, extraEnvironment, err := codeDriverCommand([]string{runtime}, config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, argv[0], argv[1:]...)
			command.Dir = directory
			command.Env = append(os.Environ(), extraEnvironment...)
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if output, err := command.StderrPipe(); err == nil {
				go func() { _, _ = io.Copy(os.Stderr, output) }()
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() {
				t.Fatalf("driver did not become ready: %v", scanner.Err())
			}
			var ready protocolFrame
			if err := json.Unmarshal(scanner.Bytes(), &ready); err != nil || ready.Type != "ready" {
				t.Fatalf("unexpected ready frame %q: %v", scanner.Bytes(), err)
			}
			if err := json.NewEncoder(stdin).Encode(protocolFrame{Protocol: applicationProtocol, Type: "invoke", ID: "one", Operation: "ask", Input: json.RawMessage(`{"prompt":"hello"}`)}); err != nil {
				t.Fatal(err)
			}
			if !scanner.Scan() {
				t.Fatalf("driver did not return a result: %v", scanner.Err())
			}
			var result protocolFrame
			if err := json.Unmarshal(scanner.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			var output map[string]string
			if err := json.Unmarshal(result.Output, &output); err != nil {
				t.Fatal(err)
			}
			if result.Error != "" || output["answer"] != "result:hello" {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

func TestStreamCollectorDelimiterAndIdleCompletion(t *testing.T) {
	collector := newStreamCollector()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		_, _ = collector.Write([]byte("partial"))
		time.Sleep(10 * time.Millisecond)
		_, _ = collector.Write([]byte(" response> "))
	}()
	start, end, err := collector.wait(ctx, func(value []byte) (int, int, bool) {
		for index := range value {
			if index+2 <= len(value) && string(value[index:index+2]) == "> " {
				return 0, index + 2, true
			}
		}
		return 0, 0, false
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(collector.slice(start, end)); got != "partial response> " {
		t.Fatalf("unexpected delimited output %q", got)
	}
	collector.consume(end)
	go func() { _, _ = collector.Write([]byte("idle result")) }()
	start, end, err = collector.wait(ctx, nil, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(collector.slice(start, end)); got != "idle result" {
		t.Fatalf("unexpected idle output %q", got)
	}
}

func TestApplicationEnvironmentAppliesSealedTLSTrustMode(t *testing.T) {
	server := &Server{blueprint: &environment.Blueprint{Network: environment.NetworkSimulation{TLS: environment.TLSConfig{Enabled: true, TrustMode: "auto"}}, Application: environment.Application{Launch: environment.Launch{Environment: map[string]string{"APP_MODE": "test"}}}}, virtualTime: time.Now().UTC()}
	values := server.applicationEnvironment(nil)
	for _, expected := range []string{"INTERCEPTOR_CA_BUNDLE=/opt/interceptor/tls/ca.pem", "SSL_CERT_FILE=/opt/interceptor/tls/ca.pem", "NODE_EXTRA_CA_CERTS=/opt/interceptor/tls/ca.pem", "APP_MODE=test"} {
		found := false
		for _, value := range values {
			found = found || value == expected
		}
		if !found {
			t.Fatalf("application environment omitted %q: %#v", expected, values)
		}
	}
}
