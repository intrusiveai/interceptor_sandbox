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
	"time"
)

type HelperRequest struct {
	Command   []string `json:"command"`
	TimeoutMS int64    `json:"timeout_ms"`
}

type HelperResponse struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Output string `json:"output,omitempty"`
}

func RunHelper() error {
	scanner := bufio.NewScanner(io.LimitReader(os.Stdin, 32<<20))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		var request HelperRequest
		if err := decoder.Decode(&request); err != nil || len(request.Command) == 0 || request.Command[0] == "" || request.TimeoutMS <= 0 || request.TimeoutMS > 10*60*1000 {
			_ = encoder.Encode(HelperResponse{Error: "invalid helper request"})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(request.TimeoutMS)*time.Millisecond)
		command := exec.CommandContext(ctx, request.Command[0], request.Command[1:]...)
		output, err := command.CombinedOutput()
		cancel()
		response := HelperResponse{OK: err == nil, Output: boundedOutput(output)}
		if err != nil {
			response.Error = err.Error()
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read helper requests: %w", err)
	}
	return nil
}

func boundedOutput(value []byte) string {
	if len(value) > 64<<10 {
		value = value[:64<<10]
	}
	return string(value)
}
