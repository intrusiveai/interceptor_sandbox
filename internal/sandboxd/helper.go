package sandboxd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type HelperRequest struct {
	ID        string   `json:"id"`
	Type      string   `json:"type,omitempty"`
	Command   []string `json:"command"`
	Stdin     []byte   `json:"stdin,omitempty"`
	TimeoutMS int64    `json:"timeout_ms"`
}

type HelperResponse struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

func RunHelper() error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	encoder := json.NewEncoder(os.Stdout)
	var encoderMu sync.Mutex
	running := map[string]*exec.Cmd{}
	var runningMu sync.Mutex
	write := func(response HelperResponse) {
		encoderMu.Lock()
		_ = encoder.Encode(response)
		encoderMu.Unlock()
	}
	for scanner.Scan() {
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		var request HelperRequest
		if err := decoder.Decode(&request); err != nil || request.ID == "" {
			write(HelperResponse{ID: request.ID, Error: "invalid helper request"})
			continue
		}
		if request.Type == "cancel" {
			runningMu.Lock()
			command := running[request.ID]
			runningMu.Unlock()
			if command != nil && command.Process != nil {
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			}
			continue
		}
		if len(request.Command) == 0 || request.Command[0] == "" || request.TimeoutMS <= 0 || request.TimeoutMS > 10*60*1000 {
			write(HelperResponse{ID: request.ID, Error: "invalid helper request"})
			continue
		}
		command := exec.Command(request.Command[0], request.Command[1:]...)
		command.Stdin = bytes.NewReader(request.Stdin)
		stdout, stderr := &boundedBuffer{limit: 4 << 20}, &boundedBuffer{limit: 4 << 20}
		command.Stdout, command.Stderr = stdout, stderr
		configureHelperChild(command)
		err := command.Start()
		if err != nil {
			write(HelperResponse{ID: request.ID, Error: err.Error()})
			continue
		}
		runningMu.Lock()
		running[request.ID] = command
		runningMu.Unlock()
		go func(request HelperRequest, command *exec.Cmd, stdout, stderr *boundedBuffer) {
			finished := make(chan error, 1)
			go func() { finished <- command.Wait() }()
			timer := time.NewTimer(time.Duration(request.TimeoutMS) * time.Millisecond)
			var err error
			select {
			case err = <-finished:
				timer.Stop()
			case <-timer.C:
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				err = <-finished
				if err == nil {
					err = context.DeadlineExceeded
				}
			}
			runningMu.Lock()
			delete(running, request.ID)
			runningMu.Unlock()
			response := HelperResponse{ID: request.ID, OK: err == nil && !stdout.overflow && !stderr.overflow, Stdout: stdout.bytes(), Stderr: stderr.bytes()}
			if command.ProcessState != nil {
				response.ExitCode = command.ProcessState.ExitCode()
			}
			if err != nil {
				response.Error = err.Error()
			} else if stdout.overflow || stderr.overflow {
				response.Error = "command output exceeded the 4 MiB stream limit"
			}
			write(response)
		}(request, command, stdout, stderr)
	}
	runningMu.Lock()
	for _, command := range running {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	runningMu.Unlock()
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read helper requests: %w", err)
	}
	return nil
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.overflow = true
		return original, nil
	}
	if len(value) > remaining {
		b.overflow = true
		value = value[:remaining]
	}
	_, _ = b.buffer.Write(value)
	return original, nil
}

func (b *boundedBuffer) bytes() []byte { return append([]byte(nil), b.buffer.Bytes()...) }
