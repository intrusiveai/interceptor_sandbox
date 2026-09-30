package ociruntime

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

const SessionLabel = "ai.intrusive.interceptor.session"

// Binding is host-owned: its directory must never be mounted into the target.
// ContainerID is empty only before create has returned and been persisted.
type Binding struct {
	Endpoint        string `json:"endpoint"`
	DaemonID        string `json:"daemon_id"`
	SessionID       string `json:"session_id"`
	ContainerID     string `json:"container_id,omitempty"`
	CreateAttempted bool   `json:"create_attempted"`
}

func BindingPath(root, sessionID string) string {
	return filepath.Join(root, "container-bindings", sessionID+".json")
}

func fullID(id string) bool {
	if len(id) != 64 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func localEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, "unix:///") && !strings.ContainsAny(endpoint, "\x00\r\n")
}

func (d Docker) command(ctx context.Context, args ...string) *exec.Cmd {
	if d.Endpoint != "" {
		args = append([]string{"--host", d.Endpoint}, args...)
	}
	cmd := exec.CommandContext(ctx, d.executable(), args...)
	if d.Endpoint != "" {
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			switch key {
			case "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH":
				continue
			}
			cmd.Env = append(cmd.Env, value)
		}
	}
	return cmd
}

// output keeps warnings on stderr from corrupting successful IDs or JSON.
func (d Docker) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := d.command(ctx, args...)
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	output, err := cmd.Output()
	if err != nil {
		return output, fmt.Errorf("%w: %s", err, strings.TrimSpace(diagnostics.String()))
	}
	return output, nil
}

// Pin resolves the administrator's selected CLI context once, accepts only a
// local Unix socket, and records the server identity. All later calls use --host.
func (d Docker) Pin(ctx context.Context) (Docker, error) {
	if d.Endpoint == "" {
		contextName := os.Getenv("DOCKER_CONTEXT")
		if contextName == "" {
			d.Endpoint = os.Getenv("DOCKER_HOST")
		}
		if d.Endpoint == "" {
			args := []string{"context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}
			if contextName != "" {
				args = append(args, contextName)
			}
			output, err := d.output(ctx, args...)
			if err != nil {
				return d, fmt.Errorf("resolve local Docker endpoint: %w: %s", err, output)
			}
			d.Endpoint = strings.TrimSpace(string(output))
		}
	}
	if !localEndpoint(d.Endpoint) {
		return d, errors.New("Interceptor requires a local Docker Unix socket")
	}
	actual, err := d.serverID(ctx)
	if err != nil {
		return d, err
	}
	if d.DaemonID != "" && d.DaemonID != actual {
		return d, errors.New("Docker daemon identity changed; termination unconfirmed")
	}
	d.DaemonID = actual
	return d, nil
}

func (d Docker) serverID(ctx context.Context) (string, error) {
	output, err := d.output(ctx, "info", "--format", "{{.ID}}")
	if err != nil {
		return "", fmt.Errorf("original Docker daemon unavailable; termination unconfirmed: %w: %s", err, output)
	}
	id := strings.TrimSpace(string(output))
	if id == "" {
		return "", errors.New("Docker daemon returned no identity")
	}
	return id, nil
}

func (d Docker) verifyDaemon(ctx context.Context) error {
	if !localEndpoint(d.Endpoint) || d.DaemonID == "" {
		return errors.New("saved local Docker binding required; termination unconfirmed")
	}
	actual, err := d.serverID(ctx)
	if err != nil {
		return err
	}
	if actual != d.DaemonID {
		return errors.New("Docker daemon identity changed; termination unconfirmed")
	}
	return nil
}

func (d Docker) PrepareBinding(root, id string) (Docker, error) {
	if !localEndpoint(d.Endpoint) || d.DaemonID == "" {
		return d, errors.New("Docker endpoint must be pinned before launch")
	}
	d.SessionID, d.bindingPath = id, BindingPath(root, id)
	if _, err := os.Stat(d.bindingPath); !os.IsNotExist(err) {
		return d, errors.New("Docker launch binding already exists or cannot be inspected")
	}
	return d, fsutil.WriteJSON(d.bindingPath, Binding{Endpoint: d.Endpoint, DaemonID: d.DaemonID, SessionID: id}, 0600)
}

// ForSession never falls back to the caller's context or to a mutable name.
// Older name-only sessions require explicit administrator reconciliation.
func (d Docker) ForSession(ctx context.Context, root, id string) (Docker, string, error) {
	path := BindingPath(root, id)
	data, err := os.ReadFile(path)
	if err != nil {
		return d, "", fmt.Errorf("session %s has no readable host Docker binding; termination unconfirmed (name-only records cannot be adopted): %w", id, err)
	}
	var b Binding
	if err := json.Unmarshal(data, &b); err != nil {
		return d, "", err
	}
	if b.SessionID != id || !localEndpoint(b.Endpoint) || b.DaemonID == "" || (b.ContainerID != "" && !fullID(b.ContainerID)) {
		return d, "", errors.New("invalid host Docker binding; termination unconfirmed")
	}
	d.Endpoint, d.DaemonID, d.SessionID, d.bindingPath = b.Endpoint, b.DaemonID, b.SessionID, path
	if b.CreateAttempted && b.ContainerID == "" {
		if err := d.verifyDaemon(ctx); err != nil {
			return d, "", err
		}
		output, err := d.command(ctx, "ps", "--all", "--no-trunc", "--filter", "label="+SessionLabel+"="+id, "--format", "{{.ID}}").CombinedOutput()
		if err != nil {
			return d, "", err
		}
		ids := strings.Fields(string(output))
		if len(ids) != 1 || !fullID(ids[0]) {
			return d, "", errors.New("Docker create outcome unconfirmed; expected exactly one session-labelled container on the saved daemon")
		}
		if _, err := d.inspectOwned(ctx, ids[0]); err != nil {
			return d, "", err
		}
		b.ContainerID = ids[0]
		if err := d.saveBinding(b.ContainerID, true); err != nil {
			return d, "", err
		}
	}
	return d, b.ContainerID, nil
}

func (d Docker) saveBinding(id string, attempted bool) error {
	return fsutil.WriteJSON(d.bindingPath, Binding{Endpoint: d.Endpoint, DaemonID: d.DaemonID, SessionID: d.SessionID, ContainerID: id, CreateAttempted: attempted}, 0600)
}

// Create leaves the container stopped until the caller persists session metadata.
func (d Docker) Create(ctx context.Context, config RunConfig) (string, error) {
	if err := d.verifyDaemon(ctx); err != nil {
		return "", err
	}
	if d.bindingPath == "" || d.SessionID == "" {
		return "", errors.New("host launch binding required")
	}
	args, err := RunArguments(config)
	if err != nil {
		return "", err
	}
	args = append([]string{"create", "--restart=no", "--label", SessionLabel + "=" + d.SessionID}, args[2:]...)
	if err := d.saveBinding("", true); err != nil {
		return "", err
	}
	output, err := d.output(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("create Docker container (outcome unconfirmed): %w: %s", err, output)
	}
	id := strings.TrimSpace(string(output))
	if !fullID(id) {
		return "", errors.New("Docker create did not return a full container ID")
	}
	// Return the ID even if persistence fails, so the live host can still stop it.
	return id, d.saveBinding(id, true)
}

func (d Docker) Start(ctx context.Context, id string) error {
	if _, err := d.inspectOwned(ctx, id); err != nil {
		return err
	}
	output, err := d.command(ctx, "start", id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start Docker container: %w: %s", err, output)
	}
	return nil
}

type containerIdentity struct {
	ID     string                             `json:"Id"`
	Config struct{ Labels map[string]string } `json:"Config"`
	State  struct{ Running bool }             `json:"State"`
}

func (d Docker) inspectOwned(ctx context.Context, id string) (containerIdentity, error) {
	var item containerIdentity
	if err := d.verifyDaemon(ctx); err != nil {
		return item, err
	}
	if !fullID(id) || d.SessionID == "" {
		return item, errors.New("full Docker ID and saved session identity required")
	}
	output, err := d.output(ctx, "container", "inspect", "--format", "{{json .}}", id)
	if err != nil {
		return item, fmt.Errorf("inspect bound container: %w: %s", err, output)
	}
	if err := json.Unmarshal(output, &item); err != nil {
		return item, err
	}
	if item.ID != id || item.Config.Labels[SessionLabel] != d.SessionID {
		return item, errors.New("Docker container/session identity mismatch; termination unconfirmed")
	}
	return item, nil
}

// The exit receipt survives process loss between removal and final metadata.
func (d Docker) recordExit() error {
	return fsutil.WriteJSON(d.bindingPath+".exited", map[string]bool{"confirmed": true}, 0600)
}
func (d Docker) exitRecorded() bool {
	data, err := os.ReadFile(d.bindingPath + ".exited")
	var result struct{ Confirmed bool }
	return err == nil && json.Unmarshal(data, &result) == nil && result.Confirmed
}
