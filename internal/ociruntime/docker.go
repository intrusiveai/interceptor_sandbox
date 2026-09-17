package ociruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Docker struct {
	Binary string
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
	command := exec.CommandContext(ctx, d.executable(), "version", "--format", "{{.Server.Version}}")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("Docker is unavailable: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d Docker) Build(ctx context.Context, contextDir, tag string) error {
	if tag == "" || contextDir == "" {
		return errors.New("Docker build context and tag are required")
	}
	command := exec.CommandContext(ctx, d.executable(), "build", "--pull=false", "--tag", tag, contextDir)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("Docker build failed: %w\n%s", err, output)
	}
	return nil
}

func (d Docker) ImageID(ctx context.Context, image string) (string, error) {
	command := exec.CommandContext(ctx, d.executable(), "image", "inspect", "--format", "{{.Id}}", image)
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

func (d Docker) Run(ctx context.Context, config RunConfig) (string, error) {
	arguments, err := RunArguments(config)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, d.executable(), arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("start Docker container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
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
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	command := exec.CommandContext(ctx, d.executable(), "stop", "--time", strconv.Itoa(seconds), id)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("stop Docker container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d Docker) Remove(ctx context.Context, id string) error {
	command := exec.CommandContext(ctx, d.executable(), "rm", id)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("remove Docker container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d Docker) Running(ctx context.Context, id string) (bool, error) {
	command := exec.CommandContext(ctx, d.executable(), "inspect", "--format", "{{.State.Running}}", id)
	output, err := command.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspect Docker container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)) == "true", nil
}

func (d Docker) Logs(ctx context.Context, id string) (string, error) {
	command := exec.CommandContext(ctx, d.executable(), "logs", id)
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
	command := exec.CommandContext(ctx, d.executable(), "ps", "--filter", "name=^/interceptor-sess-", "--format", "{{.Names}}")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list Interceptor containers: %w: %s", err, output)
	}
	return strings.Fields(string(output)), nil
}
func (d Docker) ContainerPresent(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, errors.New("container identity is missing")
	}
	command := exec.CommandContext(ctx, d.executable(), "ps", "--all", "--filter", "name=^/"+id+"$", "--format", "{{.Names}}")
	output, err := command.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("confirm container removal: %w: %s", err, output)
	}
	return strings.TrimSpace(string(output)) != "", nil
}

// Kill does not wait for cooperative shutdown or a functioning supervisor.
func (d Docker) Kill(ctx context.Context, id string) error {
	command := exec.CommandContext(ctx, d.executable(), "kill", "--signal", "KILL", id)
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
