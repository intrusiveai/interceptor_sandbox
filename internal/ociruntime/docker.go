package ociruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Docker struct {
	Binary      string
	Endpoint    string
	DaemonID    string
	SessionID   string
	bindingPath string
}

type RunConfig struct {
	Name            string
	Image           string
	User            string
	Environment     map[string]string
	Mounts          []Mount
	Hostnames       []string
	DNS             []string
	Sysctls         map[string]string
	PIDs            int64
	MemoryBytes     int64
	CPUs            string
	ReadOnly        bool
	NetworkNone     bool
	NoNewPrivileges bool
	DropAllCaps     bool
	AddCaps         []string
	TmpfsBytes      int64
}

type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

func (d Docker) executable() string {
	if d.Binary == "" {
		return "docker"
	}
	return d.Binary
}

func (d Docker) Available(ctx context.Context) error {
	command := d.command(ctx, "version", "--format", "{{.Server.Version}}")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("Docker is unavailable: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d Docker) Build(ctx context.Context, contextDir, tag string) error {
	if tag == "" || contextDir == "" {
		return errors.New("Docker build context and tag are required")
	}
	command := d.command(ctx, "build", "--pull=false", "--tag", tag, contextDir)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("Docker build failed: %w\n%s", err, output)
	}
	return nil
}

func (d Docker) ImageID(ctx context.Context, image string) (string, error) {
	command := d.command(ctx, "image", "inspect", "--format", "{{.Id}}", image)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect Docker image: %w: %s", err, strings.TrimSpace(string(output)))
	}
	value := strings.TrimSpace(string(output))
	if !strings.HasPrefix(value, "sha256:") {
		return "", fmt.Errorf("Docker returned invalid image ID %q", value)
	}
	return value, nil
}

// ImageVolumes are implicitly mounted by Docker even when RunConfig has no
// explicit bind at that path. Callers installing immutable code must check them.
func (d Docker) ImageVolumes(ctx context.Context, image string) ([]string, error) {
	output, err := d.command(ctx, "image", "inspect", "--format", "{{json .Config.Volumes}}", image).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("inspect Docker image volumes: %w: %s", err, output)
	}
	var volumes map[string]json.RawMessage
	if err := json.Unmarshal(output, &volumes); err != nil {
		return nil, fmt.Errorf("decode Docker image volumes: %w", err)
	}
	paths := make([]string, 0, len(volumes))
	for path := range volumes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// ImageUser lets build-time installers temporarily elevate without changing the
// source image's default user in the completed wrapper.
func (d Docker) ImageUser(ctx context.Context, image string) (string, error) {
	output, err := d.command(ctx, "image", "inspect", "--format", "{{json .Config.User}}", image).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect Docker image user: %w: %s", err, output)
	}
	var user string
	if err := json.Unmarshal(output, &user); err != nil {
		return "", fmt.Errorf("decode Docker image user: %w", err)
	}
	if strings.ContainsAny(user, "\r\n\x00 \t") {
		return "", errors.New("image user contains invalid whitespace")
	}
	return user, nil
}

func RunArguments(config RunConfig) ([]string, error) {
	if config.Name == "" || config.Image == "" {
		return nil, errors.New("container name and image are required")
	}
	arguments := []string{"run", "--detach", "--name", config.Name, "--init"}
	if config.NetworkNone {
		arguments = append(arguments, "--network", "none")
	}
	if config.ReadOnly {
		arguments = append(arguments, "--read-only")
	}
	if config.DropAllCaps {
		arguments = append(arguments, "--cap-drop", "ALL")
	}
	for _, capability := range config.AddCaps {
		switch capability {
		case "SETUID", "SETGID", "KILL":
			arguments = append(arguments, "--cap-add", capability)
		default:
			return nil, fmt.Errorf("unsupported container capability %q", capability)
		}
	}
	if config.NoNewPrivileges {
		arguments = append(arguments, "--security-opt", "no-new-privileges")
	}
	if config.User != "" {
		arguments = append(arguments, "--user", config.User)
	}
	if config.PIDs > 0 {
		arguments = append(arguments, "--pids-limit", strconv.FormatInt(config.PIDs, 10))
	}
	if config.MemoryBytes > 0 {
		arguments = append(arguments, "--memory", strconv.FormatInt(config.MemoryBytes, 10))
	}
	if config.CPUs != "" {
		arguments = append(arguments, "--cpus", config.CPUs)
	}
	if config.TmpfsBytes > 0 {
		arguments = append(arguments, "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,mode=1777,size="+strconv.FormatInt(config.TmpfsBytes, 10))
	}
	for _, resolver := range config.DNS {
		if resolver != "127.0.0.1" {
			return nil, fmt.Errorf("unsupported container DNS resolver %q", resolver)
		}
		arguments = append(arguments, "--dns", resolver)
	}
	for _, name := range sortedMapKeys(config.Sysctls) {
		value := config.Sysctls[name]
		if name != "net.ipv4.ip_unprivileged_port_start" || value != "53" {
			return nil, fmt.Errorf("unsupported container sysctl %s=%s", name, value)
		}
		arguments = append(arguments, "--sysctl", name+"="+value)
	}
	for _, hostname := range sortedKeys(config.Hostnames) {
		if strings.ContainsAny(hostname, ",:\t\r\n ") {
			return nil, fmt.Errorf("invalid local hostname %q", hostname)
		}
		arguments = append(arguments, "--add-host", hostname+":127.0.0.1")
	}
	for _, name := range sortedMapKeys(config.Environment) {
		if strings.ContainsRune(name, '=') || strings.ContainsAny(name, "\x00\r\n") || strings.ContainsAny(config.Environment[name], "\x00") {
			return nil, fmt.Errorf("invalid container environment entry %q", name)
		}
		arguments = append(arguments, "--env", name+"="+config.Environment[name])
	}
	for _, mount := range config.Mounts {
		if mount.Source == "" || mount.Target == "" || !strings.HasPrefix(mount.Target, "/") || strings.ContainsAny(mount.Source+mount.Target, "\x00\r\n,") {
			return nil, fmt.Errorf("invalid container mount %#v", mount)
		}
		value := "type=bind,src=" + mount.Source + ",dst=" + mount.Target
		if mount.ReadOnly {
			value += ",readonly"
		}
		arguments = append(arguments, "--mount", value)
	}
	arguments = append(arguments, config.Image, "sandboxd")
	return arguments, nil
}

func (d Docker) Stop(ctx context.Context, id string, timeout time.Duration) error {
	if _, err := d.inspectOwned(ctx, id); err != nil {
		return err
	}
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	command := d.command(ctx, "stop", "--time", strconv.Itoa(seconds), id)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("stop Docker container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d Docker) Remove(ctx context.Context, id string) error {
	item, err := d.inspectOwned(ctx, id)
	if err != nil {
		return err
	}
	if item.State.Running {
		return errors.New("refusing to remove running container")
	}
	if err := d.recordExit(); err != nil {
		return err
	}
	command := d.command(ctx, "rm", id)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("remove Docker container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d Docker) Running(ctx context.Context, id string) (bool, error) {
	item, err := d.inspectOwned(ctx, id)
	return item.State.Running, err
}

func (d Docker) Logs(ctx context.Context, id string) (string, error) {
	command := d.command(ctx, "logs", id)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	return output.String(), err
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys(values []string) []string {
	copy := append([]string(nil), values...)
	sort.Strings(copy)
	return copy
}

// InterceptorContainers returns live targets, including paused containers.
// Stopped historical containers and their evidence are left untouched.
func (d Docker) InterceptorContainers(ctx context.Context) ([]string, error) {
	if err := d.verifyDaemon(ctx); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	// Labels survive renames; the second query catches older, unlabelled targets.
	for _, filter := range []string{"label=" + SessionLabel, "name=^/interceptor-sess-"} {
		output, err := d.command(ctx, "ps", "--no-trunc", "--filter", filter, "--format", "{{.ID}}").CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("list Interceptor containers: %w: %s", err, output)
		}
		for _, id := range strings.Fields(string(output)) {
			seen[id] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}
func (d Docker) ContainerPresent(ctx context.Context, id string) (bool, error) {
	if err := d.verifyDaemon(ctx); err != nil {
		return false, err
	}
	if id == "" {
		// Only a persisted, never-attempted launch may have no ID.
		data, err := os.ReadFile(d.bindingPath)
		var b Binding
		if err != nil || json.Unmarshal(data, &b) != nil || b.CreateAttempted {
			return false, errors.New("container identity unconfirmed")
		}
		return false, nil
	}
	if !fullID(id) {
		return false, errors.New("full Docker container ID required")
	}
	output, err := d.command(ctx, "ps", "--all", "--no-trunc", "--filter", "id="+id, "--format", "{{.ID}}").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("confirm container presence: %w: %s", err, output)
	}
	ids := strings.Fields(string(output))
	if len(ids) == 0 {
		if !d.exitRecorded() {
			return false, errors.New("container absent without confirmed exit; termination unconfirmed")
		}
		return false, nil
	}
	if len(ids) != 1 || ids[0] != id {
		return false, errors.New("Docker returned mismatched container ID")
	}
	_, err = d.inspectOwned(ctx, id)
	return err == nil, err
}

// Kill does not wait for cooperative shutdown or a functioning supervisor.
func (d Docker) Kill(ctx context.Context, id string) error {
	if _, err := d.inspectOwned(ctx, id); err != nil {
		return err
	}
	command := d.command(ctx, "kill", "--signal", "KILL", id)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("kill Docker container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// EnsureRemoved verifies exit/removal even when a concurrent stop won the race.
func (d Docker) EnsureRemoved(ctx context.Context, id string, immediate bool) error {
	present, err := d.ContainerPresent(ctx, id)
	if err != nil || !present {
		return err
	}
	running, err := d.Running(ctx, id)
	if err != nil {
		return err
	}
	if running {
		if immediate {
			err = d.Kill(ctx, id)
		} else {
			err = d.Stop(ctx, id, 5*time.Second)
		}
		if err != nil {
			present, checkErr := d.ContainerPresent(ctx, id)
			if checkErr == nil && !present {
				return nil
			}
			running, checkErr = d.Running(ctx, id)
			if checkErr != nil || running {
				return err
			}
		}
	}
	if err := d.Remove(ctx, id); err != nil {
		present, checkErr := d.ContainerPresent(ctx, id)
		if checkErr != nil || present {
			return err
		}
	}
	present, err = d.ContainerPresent(ctx, id)
	if err != nil {
		return err
	}
	if present {
		return errors.New("container removal unconfirmed")
	}
	return nil
}

// WatchConnection holds a daemon event stream. Losing it is terminal even if
// Docker later returns with a live-restored container. The caller never reconnects.
func (d Docker) WatchConnection(ctx context.Context) error {
	command := d.command(ctx, "events", "--format", "{{.Action}}")
	command.WaitDelay = time.Second
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 1024)
	for scanner.Scan() {
	}
	if scanner.Err() != nil {
		_ = command.Process.Kill()
	}
	err = errors.Join(scanner.Err(), command.Wait())
	if err == nil {
		err = errors.New("Docker event stream closed")
	}
	return err
}
