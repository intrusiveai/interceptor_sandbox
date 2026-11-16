package sandboxd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

// A separate instance of app-helper stays outside the target process group, so
// filesystem reads continue while snapshot capture has SIGSTOP'd the target.
// Snapshot and oracle reads share this process. Only bounded results cross its pipes; it never receives blob-store access.
type snapshotHelper struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Reader
	mu      sync.Mutex
	done    chan struct{}
}

func (s *Server) startSnapshotHelper() error {
	needed := s.blueprint.Application.Persistence.SnapshotCapable
	for _, oracle := range s.blueprint.Oracles {
		needed = needed || oracle.Kind == "file_state_match"
	}
	if !needed || len(s.containerRoots()) == 0 {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "app-helper")
	roots, _ := json.Marshal(s.blueprint.Application.Persistence.MutableRoots)
	s.configureApplicationCommand(command, []string{"INTERCEPTOR_SNAPSHOT_ROOTS=" + string(roots)})
	input, err := command.StdinPipe()
	if err != nil {
		return err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return err
	}
	command.Stderr = io.Discard
	if err = command.Start(); err != nil {
		input.Close()
		output.Close()
		return err
	}
	h := &snapshotHelper{command: command, input: input, output: bufio.NewReader(output), done: make(chan struct{})}
	s.snapshotHelper = h
	go func() { _ = command.Wait(); close(h.done) }()
	return nil
}
func (h *snapshotHelper) close() {
	if h == nil {
		return
	}
	_ = h.input.Close()
	_ = h.command.Process.Kill()
	<-h.done
}
func (h *snapshotHelper) read(ctx context.Context, q snapshot.ReadRequest) (snapshot.ReadResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return snapshot.ReadResponse{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = h.command.Process.Kill() })
	defer stop()
	raw, err := json.Marshal(HelperRequest{ID: "snapshot", Type: "snapshot", Snapshot: &q})
	if err != nil {
		return snapshot.ReadResponse{}, err
	}
	if _, err = h.input.Write(append(raw, '\n')); err != nil {
		return snapshot.ReadResponse{}, err
	}
	// Do not let a compromised application-identity helper cause unbounded allocation.
	line, err := bufio.NewReader(io.LimitReader(h.output, 256<<10)).ReadBytes('\n')
	if err != nil {
		return snapshot.ReadResponse{}, err
	}
	var reply HelperResponse
	if err = json.Unmarshal(line, &reply); err != nil {
		return snapshot.ReadResponse{}, err
	}
	if reply.ID == "snapshot" && !reply.OK {
		switch reply.FileError {
		case "not_found":
			return snapshot.ReadResponse{}, fmt.Errorf("%w: %s", os.ErrNotExist, reply.Error)
		case "permission_denied":
			return snapshot.ReadResponse{}, fmt.Errorf("%w: %s", os.ErrPermission, reply.Error)
		}
	}
	if reply.ID != "snapshot" || !reply.OK || reply.Snapshot == nil {
		return snapshot.ReadResponse{}, fmt.Errorf("snapshot helper read failed: %s", reply.Error)
	}
	return *reply.Snapshot, nil
}
func (s *Server) readMutable(ctx context.Context, roots []snapshot.Root, q snapshot.ReadRequest) (snapshot.ReadResponse, error) {
	response, err := snapshot.RootReader(roots)(ctx, q)
	if errors.Is(err, os.ErrPermission) && s.snapshotHelper != nil {
		return s.snapshotHelper.read(ctx, q)
	}
	return response, err
}
