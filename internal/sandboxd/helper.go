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

	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

type HelperRequest struct {
	Snapshot  *snapshot.ReadRequest `json:"snapshot,omitempty"`
	ID        string                `json:"id"`
	Type      string                `json:"type,omitempty"`
	Command   []string              `json:"command"`
	Stdin     []byte                `json:"stdin,omitempty"`
	TimeoutMS int64                 `json:"timeout_ms"`
}

type HelperResponse struct {
	FileError string                 `json:"file_error,omitempty"`
	Snapshot  *snapshot.ReadResponse `json:"snapshot,omitempty"`
	ID        string                 `json:"id"`
	OK        bool                   `json:"ok"`
	Error     string                 `json:"error,omitempty"`
	Stdout    []byte                 `json:"stdout,omitempty"`
	Stderr    []byte                 `json:"stderr,omitempty"`
	ExitCode  int                    `json:"exit_code,omitempty"`
}

func RunHelper() error {
	var rootPaths []string
	if value := os.Getenv("INTERCEPTOR_SNAPSHOT_ROOTS"); value != "" {
		if err := json.Unmarshal([]byte(value), &rootPaths); err != nil {
			return err
		}
	}
	roots := make([]snapshot.Root, 0, len(rootPaths))
	for _, path := range rootPaths {
		roots = append(roots, snapshot.Root{ContainerPath: path, HostPath: path})
	}
	readSnapshot := snapshot.RootReader(roots)

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
		if request.Type == "snapshot" {
			if request.Snapshot == nil {
				write(HelperResponse{ID: request.ID, Error: "missing snapshot read"})
				continue
			}
			result, err := readSnapshot(context.Background(), *request.Snapshot)
			response := HelperResponse{ID: request.ID, OK: err == nil, Snapshot: &result}
			if err != nil {
				response.Error = err.Error()
				if errors.Is(err, os.ErrNotExist) {
					response.FileError = "not_found"
				} else if errors.Is(err, os.ErrPermission) {
					response.FileError = "permission_denied"
				}
			}
			write(response)
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
