package environment

import "testing"

func TestValidateApplicationInterfaceKinds(t *testing.T) {
	operations := []ApplicationOperation{{ID: "invoke"}}
	tests := map[string]ApplicationInterface{
		"http": {
			Adapter: "http", StartupTimeoutMS: 1000, TurnTimeoutMS: 1000,
			Operations: []ApplicationOperation{{ID: "invoke", Method: "POST", Path: "/invoke"}},
			HTTP:       &HTTPInterface{Listen: "http://127.0.0.1:8080", HealthPath: "/healthz"},
		},
		"command": {
			Adapter: "command", StartupTimeoutMS: 1000, TurnTimeoutMS: 1000, Operations: operations,
			Command: &CommandInterface{Stdin: "json", Concurrency: 2},
		},
		"stdio": {
			Adapter: "stdio", StartupTimeoutMS: 1000, TurnTimeoutMS: 1000, Operations: operations,
			Stdio: &StdioInterface{Transport: "pipes", ResponseMode: "delimiter", Delimiter: "\nDONE\n"},
		},
		"repl": {
			Adapter: "repl", StartupTimeoutMS: 1000, TurnTimeoutMS: 1000, Operations: operations,
			Stdio: &StdioInterface{Transport: "pty", ResponseMode: "prompt", TerminalPattern: `agent> $`},
		},
		"external": {
			Adapter: "external", StartupTimeoutMS: 1000, TurnTimeoutMS: 1000, Operations: operations,
			External: &ExternalInterface{Protocol: "interceptor.application/v1", Concurrency: 4},
		},
		"python code": {
			Adapter: "code", StartupTimeoutMS: 1000, TurnTimeoutMS: 1000, Operations: operations,
			Code: &CodeInterface{Language: "python", Module: "agent", Invoke: "invoke", InputMode: "single"},
		},
		"node code": {
			Adapter: "code", StartupTimeoutMS: 1000, TurnTimeoutMS: 1000, Operations: operations,
			Code: &CodeInterface{Language: "node", Module: "./agent.mjs", Invoke: "invoke", InputMode: "single"},
		},
	}
	for name, binding := range tests {
		t.Run(name, func(t *testing.T) {
			if failures := validateApplicationInterface(binding); len(failures) != 0 {
				t.Fatalf("valid interface rejected: %v", failures)
			}
		})
	}
}

func TestValidateApplicationInterfaceRejectsMixedConfiguration(t *testing.T) {
	binding := ApplicationInterface{
		Adapter: "http", StartupTimeoutMS: 1000, TurnTimeoutMS: 1000,
		Operations: []ApplicationOperation{{ID: "invoke", Method: "POST", Path: "/invoke"}},
		HTTP:       &HTTPInterface{Listen: "http://127.0.0.1:8080"},
		Command:    &CommandInterface{},
	}
	if failures := validateApplicationInterface(binding); len(failures) == 0 {
		t.Fatal("mixed adapter configuration was accepted")
	}
}
