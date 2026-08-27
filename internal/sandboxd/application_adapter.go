package sandboxd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

const applicationProtocol = "interceptor.application/v1"

type applicationInvocation struct {
	TurnID        string
	Operation     string
	Method        string
	Path          string
	Headers       map[string]string
	Body          []byte
	MediaType     string
	IdentityToken string
}

type applicationResult struct {
	Code      int
	Headers   map[string]string
	Body      []byte
	MediaType string
	ExitCode  *int
}

type applicationAdapter interface {
	Start() error
	WaitReady(context.Context) error
	Invoke(context.Context, applicationInvocation) (applicationResult, error)
	Handler() http.Handler
	Kind() string
}

func (s *Server) newApplicationAdapter() (applicationAdapter, error) {
	switch s.blueprint.Application.Interface.Adapter {
	case "http":
		return newHTTPApplicationAdapter(s)
	case "command":
		concurrency := s.blueprint.Application.Interface.Command.Concurrency
		if concurrency == 0 {
			concurrency = 1
		}
		return &commandApplicationAdapter{server: s, limit: make(chan struct{}, concurrency)}, nil
	case "stdio", "repl":
		return newStdioApplicationAdapter(s)
	case "external":
		return newProtocolApplicationAdapter(s, s.blueprint.Application.Launch.Command, nil)
	case "code":
		command, env, err := codeDriverCommand(s.blueprint.Application.Launch.Command, s.blueprint.Application.Interface.Code)
		if err != nil {
			return nil, err
		}
		return newProtocolApplicationAdapter(s, command, env)
	default:
		return nil, fmt.Errorf("unsupported application interface %q", s.blueprint.Application.Interface.Adapter)
	}
}

func (s *Server) operation(id string) (environment.ApplicationOperation, bool) {
	for _, operation := range s.blueprint.Application.Interface.Operations {
		if operation.ID == id {
			return operation, true
		}
	}
	return environment.ApplicationOperation{}, false
}

func (s *Server) routeOperation(method, path string) (environment.ApplicationOperation, bool) {
	for _, operation := range s.blueprint.Application.Interface.Operations {
		if strings.EqualFold(operation.Method, method) && operation.Path == path {
			return operation, true
		}
	}
	return environment.ApplicationOperation{}, false
}

func (s *Server) adapterHandler(invoke func(context.Context, applicationInvocation) (applicationResult, error)) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !strings.HasPrefix(request.URL.Path, "/operations/") {
			writeError(writer, http.StatusNotFound, "unsupported_operation", "use POST /operations/{operation-id}")
			return
		}
		operationID := strings.TrimPrefix(request.URL.Path, "/operations/")
		if operationID == "" || strings.Contains(operationID, "/") {
			writeError(writer, http.StatusNotFound, "unsupported_operation", "operation is not declared")
			return
		}
		if _, ok := s.operation(operationID); !ok {
			writeError(writer, http.StatusNotFound, "unsupported_operation", "operation is not declared")
			return
		}
		body, err := readBounded(request.Body, s.blueprint.Limits.MaxBlobBytes)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		if !s.reserveTurn() {
			writeError(writer, http.StatusTooManyRequests, "turn_limit", "turn limit is exhausted")
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), s.turnTimeout())
		defer cancel()
		invocationID := fmt.Sprintf("request-%d", s.nextInvocation.Add(1))
		result, err := invoke(ctx, applicationInvocation{TurnID: invocationID, Operation: operationID, Body: body, MediaType: request.Header.Get("Content-Type")})
		if err != nil {
			s.recordDirectInvocation(invocationID, operationID, 0, nil, err)
			writeError(writer, http.StatusBadGateway, "application_error", err.Error())
			return
		}
		s.recordDirectInvocation(invocationID, operationID, result.Code, result.Body, nil)
		for name, value := range result.Headers {
			writer.Header().Set(name, value)
		}
		if result.MediaType != "" {
			writer.Header().Set("Content-Type", result.MediaType)
		}
		code := result.Code
		if code == 0 {
			code = http.StatusOK
		}
		writer.WriteHeader(code)
		_, _ = writer.Write(result.Body)
	})
}

func (s *Server) turnTimeout() time.Duration {
	value := time.Duration(s.blueprint.Application.Interface.TurnTimeoutMS) * time.Millisecond
	if value <= 0 {
		return 2 * time.Minute
	}
	return value
}

func (s *Server) startupTimeout() time.Duration {
	value := time.Duration(s.blueprint.Application.Interface.StartupTimeoutMS) * time.Millisecond
	if value <= 0 {
		return 30 * time.Second
	}
	return value
}

func (s *Server) applicationEnvironment(extra []string) []string {
	result := make([]string, 0, len(s.blueprint.Application.Launch.Environment)+len(s.blueprint.Credentials)+len(s.identityEnv)+len(extra)+1)
	for name, value := range s.blueprint.Application.Launch.Environment {
		result = append(result, name+"="+value)
	}
	for _, credential := range s.blueprint.Credentials {
		result = append(result, credential.EnvironmentVariable+"="+credential.Value)
	}
	for name, value := range s.identityEnv {
		result = append(result, name+"="+value)
	}
	s.virtualMu.RLock()
	result = append(result, "INTERCEPTOR_VIRTUAL_TIME="+s.virtualTime.Format(time.RFC3339Nano))
	s.virtualMu.RUnlock()
	return append(result, extra...)
}

func (s *Server) configureApplicationCommand(command *exec.Cmd, extraEnvironment []string) {
	command.Dir = s.blueprint.Application.Launch.Workdir
	command.Env = s.applicationEnvironment(extraEnvironment)
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: s.appUID, Gid: s.appGID}, Setpgid: true}
}

func (s *Server) startApplicationProcess(command *exec.Cmd, stdout, stderr io.WriteCloser) error {
	oldUmask := syscall.Umask(0o007)
	err := command.Start()
	syscall.Umask(oldUmask)
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return err
	}
	s.app = command
	go func() {
		err := command.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		select {
		case s.done <- err:
		default:
		}
	}()
	return nil
}

func (s *Server) openApplicationLogs() (*os.File, *os.File, error) {
	stdout, err := os.OpenFile(s.paths.Logs+"/application.stdout.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, err
	}
	stderr, err := os.OpenFile(s.paths.Logs+"/application.stderr.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, err
	}
	return stdout, stderr, nil
}

func (s *Server) appendApplicationLogs(stdout, stderr []byte) {
	for path, value := range map[string][]byte{
		s.paths.Logs + "/application.stdout.log": stdout,
		s.paths.Logs + "/application.stderr.log": stderr,
	} {
		if len(value) == 0 {
			continue
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err == nil {
			_, _ = file.Write(value)
			_ = file.Close()
		}
	}
}

type httpApplicationAdapter struct {
	server *Server
	target *url.URL
	proxy  *httputil.ReverseProxy
}

func newHTTPApplicationAdapter(server *Server) (*httpApplicationAdapter, error) {
	target, err := url.Parse(server.blueprint.Application.Interface.HTTP.Listen)
	if err != nil {
		return nil, err
	}
	return &httpApplicationAdapter{server: server, target: target, proxy: httputil.NewSingleHostReverseProxy(target)}, nil
}

func (a *httpApplicationAdapter) Kind() string { return "http" }

func (a *httpApplicationAdapter) Start() error {
	stdout, stderr, err := a.server.openApplicationLogs()
	if err != nil {
		return err
	}
	argv := a.server.blueprint.Application.Launch.Command
	command := exec.Command(argv[0], argv[1:]...)
	a.server.configureApplicationCommand(command, nil)
	command.Stdout, command.Stderr = stdout, stderr
	if err := a.server.startApplicationProcess(command, stdout, stderr); err != nil {
		return fmt.Errorf("start application: %w", err)
	}
	a.server.appTarget = a.target
	return nil
}

func (a *httpApplicationAdapter) WaitReady(ctx context.Context) error {
	path := a.server.blueprint.Application.Interface.HTTP.HealthPath
	if path == "" {
		return nil
	}
	deadline := time.Now().Add(a.server.startupTimeout())
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		requestURL := *a.target
		requestURL.Path = path
		response, err := client.Get(requestURL.String())
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-a.server.done:
			return fmt.Errorf("application exited during startup: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("application health check timed out")
}

func (a *httpApplicationAdapter) Invoke(ctx context.Context, invocation applicationInvocation) (applicationResult, error) {
	method, path := invocation.Method, invocation.Path
	if invocation.Operation != "" {
		operation, ok := a.server.operation(invocation.Operation)
		if !ok {
			return applicationResult{}, fmt.Errorf("unknown operation %q", invocation.Operation)
		}
		method, path = operation.Method, operation.Path
	}
	if _, ok := a.server.routeOperation(method, path); !ok {
		return applicationResult{}, fmt.Errorf("route %s %s is not declared", method, path)
	}
	target := *a.target
	target.Path, target.RawQuery = path, ""
	request, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), target.String(), bytes.NewReader(invocation.Body))
	if err != nil {
		return applicationResult{}, err
	}
	for name, value := range invocation.Headers {
		if allowedTurnHeader(name) {
			request.Header.Set(name, value)
		}
	}
	if invocation.IdentityToken != "" && a.server.blueprint.Identity.Inbound != nil {
		header := a.server.blueprint.Identity.Inbound.ApplicationHeader
		if header == "" {
			header = "Authorization"
		}
		request.Header.Set(header, "Bearer "+invocation.IdentityToken)
	}
	if invocation.MediaType != "" {
		request.Header.Set("Content-Type", invocation.MediaType)
	}
	if invocation.TurnID != "" {
		request.Header.Set("X-Interceptor-Turn-ID", invocation.TurnID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return applicationResult{}, err
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body, a.server.blueprint.Limits.MaxBlobBytes)
	return applicationResult{Code: response.StatusCode, Headers: selectedHeaders(response.Header), Body: body, MediaType: response.Header.Get("Content-Type")}, err
}

func (a *httpApplicationAdapter) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		operation, ok := a.server.routeOperation(request.Method, request.URL.Path)
		if !ok {
			writeError(writer, http.StatusNotImplemented, "unsupported_route", "route is not declared by the Sealed Environment")
			return
		}
		caller := request.Header.Get("X-Interceptor-Caller")
		request.Header.Del("X-Interceptor-Caller")
		invocationID := fmt.Sprintf("request-%d", a.server.nextInvocation.Add(1))
		identityToken, identityErr := a.server.issueInboundCaller(caller, invocationID)
		if identityErr != nil {
			writeError(writer, http.StatusBadRequest, "invalid_caller_identity", identityErr.Error())
			return
		}
		if identityToken != "" {
			header := a.server.blueprint.Identity.Inbound.ApplicationHeader
			if header == "" {
				header = "Authorization"
			}
			request.Header.Set(header, "Bearer "+identityToken)
		} else if a.server.blueprint.Identity.Inbound != nil {
			header := a.server.blueprint.Identity.Inbound.ApplicationHeader
			if header == "" {
				header = "Authorization"
			}
			request.Header.Del(header)
		}
		if !a.server.reserveTurn() {
			writeError(writer, http.StatusTooManyRequests, "turn_limit", "turn limit is exhausted")
			return
		}
		request.Header.Set("X-Interceptor-Turn-ID", invocationID)
		recorder := &statusResponseWriter{ResponseWriter: writer, status: http.StatusOK}
		a.proxy.ServeHTTP(recorder, request)
		a.server.recordDirectInvocation(invocationID, operation.ID, recorder.status, nil, nil)
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) recordDirectInvocation(id, operation string, code int, body []byte, invocationError error) {
	errorText := ""
	if invocationError != nil {
		errorText = invocationError.Error()
	}
	_, _ = s.recorder.Append(recording.Draft{
		Channel: recording.Attack, Layer: "application_bridge", Kind: "application_invocation_completed",
		Provenance: recording.Application, TurnID: id,
		Semantic: map[string]any{"operation": operation, "code": code, "body": string(body), "error": errorText},
	})
	_ = s.recorder.Flush()
}

type commandApplicationAdapter struct {
	server *Server
	limit  chan struct{}
}

func (a *commandApplicationAdapter) Kind() string                    { return "command" }
func (a *commandApplicationAdapter) Start() error                    { return a.server.startApplicationHelper(true) }
func (a *commandApplicationAdapter) WaitReady(context.Context) error { return nil }
func (a *commandApplicationAdapter) Handler() http.Handler {
	return a.server.adapterHandler(a.Invoke)
}

func (a *commandApplicationAdapter) Invoke(ctx context.Context, invocation applicationInvocation) (applicationResult, error) {
	select {
	case a.limit <- struct{}{}:
		defer func() { <-a.limit }()
	case <-ctx.Done():
		return applicationResult{}, ctx.Err()
	}
	operation, ok := a.server.operation(invocation.Operation)
	if !ok {
		return applicationResult{}, fmt.Errorf("unknown operation %q", invocation.Operation)
	}
	arguments, err := bindOperationArguments(operation.Arguments, invocation.Body)
	if err != nil {
		return applicationResult{}, err
	}
	command := append(append([]string{}, a.server.blueprint.Application.Launch.Command...), arguments...)
	stdinMode := a.server.blueprint.Application.Interface.Command.Stdin
	if stdinMode == "" {
		stdinMode = "raw"
	}
	stdin := invocation.Body
	if stdinMode == "none" {
		stdin = nil
	} else if stdinMode == "json" && !json.Valid(stdin) {
		return applicationResult{}, errors.New("command input must be valid JSON")
	}
	response, err := a.server.callHelper(ctx, HelperRequest{Command: command, Stdin: stdin, TimeoutMS: a.server.turnTimeout().Milliseconds()})
	a.server.appendApplicationLogs(response.Stdout, response.Stderr)
	if err != nil {
		return applicationResult{}, err
	}
	if response.Error != "" && response.ExitCode == 0 {
		return applicationResult{}, errors.New(response.Error)
	}
	successCodes := a.server.blueprint.Application.Interface.Command.SuccessCodes
	if len(successCodes) == 0 {
		successCodes = []int{0}
	}
	if !containsInt(successCodes, response.ExitCode) {
		return applicationResult{}, fmt.Errorf("application command exited with %d: %s", response.ExitCode, strings.TrimSpace(string(response.Stderr)))
	}
	code := response.ExitCode
	mediaType := a.server.blueprint.Application.Interface.Command.OutputMediaType
	if mediaType == "" {
		mediaType = "text/plain; charset=utf-8"
	}
	return applicationResult{Code: http.StatusOK, Body: response.Stdout, MediaType: mediaType, ExitCode: &code}, nil
}

func bindOperationArguments(bindings []environment.ArgumentBinding, body []byte) ([]string, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, errors.New("operation arguments require a JSON input document")
	}
	result := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Literal != "" {
			result = append(result, binding.Literal)
			continue
		}
		value, ok := jsonPointer(document, binding.From)
		if !ok {
			return nil, fmt.Errorf("input has no value at %s", binding.From)
		}
		switch typed := value.(type) {
		case string:
			result = append(result, typed)
		case float64, bool, nil:
			result = append(result, fmt.Sprint(typed))
		default:
			encoded, _ := json.Marshal(typed)
			result = append(result, string(encoded))
		}
	}
	return result, nil
}

func jsonPointer(document any, pointer string) (any, bool) {
	if pointer == "" {
		return document, true
	}
	current := document
	for _, raw := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = typed[segment]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, false
			}
			current = typed[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

type stdioApplicationAdapter struct {
	server   *Server
	config   environment.StdioInterface
	input    io.WriteCloser
	output   *streamCollector
	invokeMu sync.Mutex
}

func newStdioApplicationAdapter(server *Server) (*stdioApplicationAdapter, error) {
	return &stdioApplicationAdapter{server: server, config: *server.blueprint.Application.Interface.Stdio, output: newStreamCollector()}, nil
}

func (a *stdioApplicationAdapter) Kind() string {
	return a.server.blueprint.Application.Interface.Adapter
}

func (a *stdioApplicationAdapter) Start() error {
	stderrLog, err := os.OpenFile(a.server.paths.Logs+"/application.stderr.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	stdoutLog, err := os.OpenFile(a.server.paths.Logs+"/application.stdout.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		_ = stderrLog.Close()
		return err
	}
	argv := a.server.blueprint.Application.Launch.Command
	command := exec.Command(argv[0], argv[1:]...)
	a.server.configureApplicationCommand(command, nil)
	transport := a.config.Transport
	if transport == "" {
		if a.Kind() == "repl" {
			transport = "pty"
		} else {
			transport = "pipes"
		}
	}
	var stdout io.ReadCloser
	if transport == "pty" {
		input, output, cleanup, err := attachPTY(command)
		if err != nil {
			_ = stdoutLog.Close()
			_ = stderrLog.Close()
			return err
		}
		a.input, stdout = input, output
		command.Stderr = stderrLog
		defer cleanup()
	} else {
		a.input, err = command.StdinPipe()
		if err == nil {
			stdout, err = command.StdoutPipe()
		}
		command.Stderr = stderrLog
		if err != nil {
			_ = stdoutLog.Close()
			_ = stderrLog.Close()
			return err
		}
	}
	tee := io.TeeReader(stdout, stdoutLog)
	go func() { _, _ = io.Copy(a.output, tee); a.output.close() }()
	if err := a.server.startApplicationProcess(command, stdoutLog, stderrLog); err != nil {
		return fmt.Errorf("start application: %w", err)
	}
	return nil
}

func (a *stdioApplicationAdapter) WaitReady(ctx context.Context) error {
	if a.config.ReadyPattern == "" {
		return nil
	}
	pattern := regexp.MustCompile(a.config.ReadyPattern)
	_, end, err := a.output.wait(ctx, func(value []byte) (int, int, bool) {
		location := pattern.FindIndex(value)
		if location == nil {
			return 0, 0, false
		}
		return location[0], location[1], true
	}, 0)
	if err == nil {
		a.output.consume(end)
	}
	return err
}

func (a *stdioApplicationAdapter) Invoke(ctx context.Context, invocation applicationInvocation) (applicationResult, error) {
	if _, ok := a.server.operation(invocation.Operation); !ok {
		return applicationResult{}, fmt.Errorf("unknown operation %q", invocation.Operation)
	}
	a.invokeMu.Lock()
	defer a.invokeMu.Unlock()
	a.output.consumeAll()
	suffix := a.config.InputSuffix
	if suffix == "" {
		suffix = "\n"
	}
	input := append(append([]byte{}, invocation.Body...), []byte(suffix)...)
	if _, err := a.input.Write(input); err != nil {
		return applicationResult{}, err
	}
	start, end, err := a.waitResponse(ctx, input)
	if err != nil {
		return applicationResult{}, err
	}
	value := a.output.slice(start, end)
	a.output.consume(end)
	if a.config.StripEcho {
		value = stripStreamEcho(value, input)
	}
	if a.config.StripTerminal {
		if a.config.ResponseMode == "delimiter" {
			value = bytes.TrimSuffix(value, []byte(a.config.Delimiter))
		} else if a.config.ResponseMode == "prompt" {
			pattern := regexp.MustCompile(a.config.TerminalPattern)
			if location := pattern.FindIndex(value); location != nil && location[1] == len(value) {
				value = value[:location[0]]
			}
		}
	}
	mediaType := a.config.OutputMediaType
	if mediaType == "" {
		mediaType = "text/plain; charset=utf-8"
	}
	return applicationResult{Code: http.StatusOK, Body: value, MediaType: mediaType}, nil
}

func (a *stdioApplicationAdapter) waitResponse(ctx context.Context, input []byte) (int, int, error) {
	mode := a.config.ResponseMode
	if mode == "" {
		mode = "line"
	}
	switch mode {
	case "line":
		return a.output.wait(ctx, func(value []byte) (int, int, bool) {
			offset := responseOffset(value, input, a.config.StripEcho)
			if index := bytes.IndexByte(value[offset:], '\n'); index >= 0 {
				return 0, offset + index + 1, true
			}
			return 0, 0, false
		}, 0)
	case "delimiter":
		delimiter := []byte(a.config.Delimiter)
		return a.output.wait(ctx, func(value []byte) (int, int, bool) {
			offset := responseOffset(value, input, a.config.StripEcho)
			if index := bytes.Index(value[offset:], delimiter); index >= 0 {
				return 0, offset + index + len(delimiter), true
			}
			return 0, 0, false
		}, 0)
	case "prompt":
		pattern := regexp.MustCompile(a.config.TerminalPattern)
		return a.output.wait(ctx, func(value []byte) (int, int, bool) {
			offset := responseOffset(value, input, a.config.StripEcho)
			locations := pattern.FindAllIndex(value[offset:], -1)
			if len(locations) == 0 {
				return 0, 0, false
			}
			last := locations[len(locations)-1]
			return 0, offset + last[1], offset+last[1] == len(value)
		}, 0)
	case "idle":
		idle := time.Duration(a.config.IdleMS) * time.Millisecond
		if idle <= 0 {
			idle = 100 * time.Millisecond
		}
		return a.output.wait(ctx, nil, idle)
	default:
		return 0, 0, fmt.Errorf("unsupported response mode %q", mode)
	}
}

func responseOffset(value, input []byte, stripEcho bool) int {
	if !stripEcho {
		return 0
	}
	if bytes.HasPrefix(value, input) {
		return len(input)
	}
	ptyInput := bytes.ReplaceAll(input, []byte("\n"), []byte("\r\n"))
	if bytes.HasPrefix(value, ptyInput) {
		return len(ptyInput)
	}
	return 0
}

func stripStreamEcho(value, input []byte) []byte {
	if bytes.HasPrefix(value, input) {
		return bytes.TrimPrefix(value, input)
	}
	return bytes.TrimPrefix(value, bytes.ReplaceAll(input, []byte("\n"), []byte("\r\n")))
}

func (a *stdioApplicationAdapter) Handler() http.Handler { return a.server.adapterHandler(a.Invoke) }

type streamCollector struct {
	mu      sync.Mutex
	buffer  []byte
	changed chan struct{}
	closed  bool
}

func newStreamCollector() *streamCollector { return &streamCollector{changed: make(chan struct{}, 1)} }

func (c *streamCollector) Write(value []byte) (int, error) {
	c.mu.Lock()
	if len(c.buffer)+len(value) > 8<<20 {
		c.mu.Unlock()
		return 0, errors.New("application stream exceeded 8 MiB")
	}
	c.buffer = append(c.buffer, value...)
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return len(value), nil
}

func (c *streamCollector) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *streamCollector) consumeAll() { c.mu.Lock(); c.buffer = nil; c.mu.Unlock() }
func (c *streamCollector) consume(end int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if end >= len(c.buffer) {
		c.buffer = nil
		return
	}
	c.buffer = append([]byte(nil), c.buffer[end:]...)
}
func (c *streamCollector) slice(start, end int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if start < 0 || end < start || end > len(c.buffer) {
		return nil
	}
	return append([]byte(nil), c.buffer[start:end]...)
}

func (c *streamCollector) wait(ctx context.Context, match func([]byte) (int, int, bool), idle time.Duration) (int, int, error) {
	var timer *time.Timer
	var timerChannel <-chan time.Time
	lastLength := -1
	for {
		c.mu.Lock()
		value, closed := append([]byte(nil), c.buffer...), c.closed
		c.mu.Unlock()
		if match != nil {
			if start, end, ok := match(value); ok {
				return start, end, nil
			}
		} else if len(value) > 0 && len(value) == lastLength {
			return 0, len(value), nil
		}
		if closed {
			return 0, 0, io.EOF
		}
		if idle > 0 && len(value) != lastLength {
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(idle)
			timerChannel = timer.C
			lastLength = len(value)
		}
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-c.changed:
		case <-timerChannel:
			c.mu.Lock()
			end := len(c.buffer)
			c.mu.Unlock()
			if end > 0 {
				return 0, end, nil
			}
		}
	}
}

type protocolFrame struct {
	Protocol  string          `json:"protocol,omitempty"`
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Operation string          `json:"operation,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
	Config    map[string]any  `json:"config,omitempty"`
}

type protocolApplicationAdapter struct {
	server    *Server
	command   []string
	extraEnv  []string
	input     io.WriteCloser
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan protocolFrame
	ready     chan error
	readyOnce sync.Once
	limit     chan struct{}
}

func newProtocolApplicationAdapter(server *Server, command, extraEnv []string) (*protocolApplicationAdapter, error) {
	if len(command) == 0 {
		return nil, errors.New("protocol adapter requires an application command")
	}
	concurrency := 1
	if server.blueprint.Application.Interface.External != nil && server.blueprint.Application.Interface.External.Concurrency > 0 {
		concurrency = server.blueprint.Application.Interface.External.Concurrency
	}
	if server.blueprint.Application.Interface.Code != nil && server.blueprint.Application.Interface.Code.Concurrency > 0 {
		concurrency = server.blueprint.Application.Interface.Code.Concurrency
	}
	return &protocolApplicationAdapter{server: server, command: command, extraEnv: extraEnv, pending: map[string]chan protocolFrame{}, ready: make(chan error, 1), limit: make(chan struct{}, concurrency)}, nil
}

func (a *protocolApplicationAdapter) Kind() string {
	return a.server.blueprint.Application.Interface.Adapter
}

func (a *protocolApplicationAdapter) Start() error {
	stdoutLog, stderrLog, err := a.server.openApplicationLogs()
	if err != nil {
		return err
	}
	command := exec.Command(a.command[0], a.command[1:]...)
	a.server.configureApplicationCommand(command, a.extraEnv)
	a.input, err = command.StdinPipe()
	if err != nil {
		return err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	command.Stderr = stderrLog
	if err := a.server.startApplicationProcess(command, stdoutLog, stderrLog); err != nil {
		return err
	}
	go a.readFrames(output, stdoutLog)
	config := map[string]any{}
	if external := a.server.blueprint.Application.Interface.External; external != nil {
		for key, value := range external.Config {
			config[key] = value
		}
		if external.ConfigAsset != "" {
			value, readErr := os.ReadFile(containerEnvironment + "/" + external.ConfigAsset)
			if readErr != nil {
				return readErr
			}
			config["config_asset_base64"] = base64.StdEncoding.EncodeToString(value)
			config["asset_path"] = external.ConfigAsset
		}
	}
	return a.writeFrame(protocolFrame{Protocol: applicationProtocol, Type: "hello", Config: config})
}

func (a *protocolApplicationAdapter) readFrames(reader io.Reader, log io.Writer) {
	scanner := bufio.NewScanner(io.TeeReader(reader, log))
	scanner.Buffer(make([]byte, 64<<10), int(min(a.server.blueprint.Limits.MaxBlobBytes+1<<20, 64<<20)))
	for scanner.Scan() {
		var frame protocolFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			a.readyOnce.Do(func() { a.ready <- fmt.Errorf("invalid application protocol frame: %w", err) })
			continue
		}
		switch frame.Type {
		case "ready":
			a.readyOnce.Do(func() { a.ready <- nil })
		case "result":
			a.pendingMu.Lock()
			channel := a.pending[frame.ID]
			a.pendingMu.Unlock()
			if channel != nil {
				select {
				case channel <- frame:
				default:
				}
			}
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	a.readyOnce.Do(func() { a.ready <- err })
	a.pendingMu.Lock()
	for _, channel := range a.pending {
		select {
		case channel <- protocolFrame{Type: "result", Error: err.Error()}:
		default:
		}
	}
	a.pendingMu.Unlock()
}

func (a *protocolApplicationAdapter) writeFrame(frame protocolFrame) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return json.NewEncoder(a.input).Encode(frame)
}

func (a *protocolApplicationAdapter) WaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-a.ready:
		return err
	}
}

func (a *protocolApplicationAdapter) Invoke(ctx context.Context, invocation applicationInvocation) (applicationResult, error) {
	select {
	case a.limit <- struct{}{}:
		defer func() { <-a.limit }()
	case <-ctx.Done():
		return applicationResult{}, ctx.Err()
	}
	if _, ok := a.server.operation(invocation.Operation); !ok {
		return applicationResult{}, fmt.Errorf("unknown operation %q", invocation.Operation)
	}
	id := invocation.TurnID
	if id == "" {
		id = fmt.Sprintf("invoke-%d", a.server.nextInvocation.Add(1))
	}
	input := json.RawMessage(invocation.Body)
	if len(input) == 0 {
		input = json.RawMessage("null")
	}
	if !json.Valid(input) {
		encoded, _ := json.Marshal(string(input))
		input = encoded
	}
	channel := make(chan protocolFrame, 1)
	a.pendingMu.Lock()
	a.pending[id] = channel
	a.pendingMu.Unlock()
	defer func() { a.pendingMu.Lock(); delete(a.pending, id); a.pendingMu.Unlock() }()
	if err := a.writeFrame(protocolFrame{Protocol: applicationProtocol, Type: "invoke", ID: id, Operation: invocation.Operation, Input: input, MediaType: invocation.MediaType}); err != nil {
		return applicationResult{}, err
	}
	select {
	case <-ctx.Done():
		_ = a.writeFrame(protocolFrame{Protocol: applicationProtocol, Type: "cancel", ID: id})
		return applicationResult{}, ctx.Err()
	case frame := <-channel:
		if frame.Error != "" {
			return applicationResult{}, errors.New(frame.Error)
		}
		output := []byte(frame.Output)
		if len(output) == 0 {
			output = []byte("null")
		}
		return applicationResult{Code: http.StatusOK, Body: output, MediaType: valueOr(frame.MediaType, "application/json")}, nil
	}
}

func (a *protocolApplicationAdapter) Handler() http.Handler { return a.server.adapterHandler(a.Invoke) }

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func codeDriverCommand(base []string, config *environment.CodeInterface) ([]string, []string, error) {
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, nil, err
	}
	extra := []string{"INTERCEPTOR_CODE_CONFIG=" + string(encoded)}
	if config.Language == "python" {
		return append(append([]string{}, base...), "-u", "-c", pythonCodeDriver), extra, nil
	}
	return append(append([]string{}, base...), "-e", nodeCodeDriver), extra, nil
}

const pythonCodeDriver = `
import asyncio, importlib, inspect, json, os, sys
cfg=json.loads(os.environ['INTERCEPTOR_CODE_CONFIG']); mod=importlib.import_module(cfg['module']); target=mod
init=cfg.get('initialize')
if init:
    fn=getattr(mod,init); target=fn(cfg.get('init_arguments') or {})
    if inspect.isawaitable(target): target=asyncio.run(target)
print(json.dumps({'protocol':'interceptor.application/v1','type':'ready'}),flush=True)
for line in sys.stdin:
    try:
        f=json.loads(line)
        if f.get('type')!='invoke': continue
        fn=getattr(target,cfg['invoke']); value=f.get('input')
        result=fn(**value) if cfg.get('input_mode')=='kwargs' else fn(value)
        if inspect.isawaitable(result): result=asyncio.run(result)
        print(json.dumps({'protocol':'interceptor.application/v1','type':'result','id':f['id'],'output':result},default=str),flush=True)
    except Exception as e: print(json.dumps({'protocol':'interceptor.application/v1','type':'result','id':f.get('id'),'error':str(e)}),flush=True)
`

const nodeCodeDriver = `
const readline=require('readline'); const cfg=JSON.parse(process.env.INTERCEPTOR_CODE_CONFIG);
(async()=>{ const mod=await import(cfg.module); let target=mod; if(cfg.initialize){ target=await mod[cfg.initialize](cfg.init_arguments||{}); }
console.log(JSON.stringify({protocol:'interceptor.application/v1',type:'ready'}));
readline.createInterface({input:process.stdin}).on('line',async line=>{ let f={}; try { f=JSON.parse(line); if(f.type!=='invoke')return; const fn=target[cfg.invoke]; const out=await fn.call(target,f.input); console.log(JSON.stringify({protocol:'interceptor.application/v1',type:'result',id:f.id,output:out})); } catch(e){ console.log(JSON.stringify({protocol:'interceptor.application/v1',type:'result',id:f.id,error:String(e.message||e)})); }}); })().catch(e=>{console.error(e);process.exit(1)});
`
