package appimage

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const companionName = "interceptor-linux"

// These fields extend the existing release.json, not a second manifest.
type releaseMetadata struct {
	Version        string `json:"version"`
	SourceCommit   string `json:"source_commit"`
	Platform       string `json:"platform"`
	BinarySHA256   string `json:"binary_sha256"`
	LinuxCompanion *struct {
		Filename string `json:"filename"`
		Platform string `json:"platform"`
		SHA256   string `json:"sha256"`
	} `json:"linux_companion"`
}

func copyLinuxBinary(executable, destination, hostOS, arch, version, source string) error {
	if hostOS == "linux" {
		data, err := os.ReadFile(executable)
		if err != nil {
			return fmt.Errorf("read running Linux interceptor: %w", err)
		}
		return os.WriteFile(destination, data, 0555)
	}
	if hostOS != "darwin" {
		return fmt.Errorf("wrapper builds are unsupported on %s", hostOS)
	}
	data, err := packagedLinuxBinary(executable, arch, version, source)
	if err != nil {
		return fmt.Errorf("packaged Linux companion: %w; re-extract the complete darwin-%s release archive for interceptor %s and keep interceptor, interceptor-linux and release.json together", err, arch, version)
	}
	return os.WriteFile(destination, data, 0555)
}

func packagedLinuxBinary(executable, arch, version, source string) ([]byte, error) {
	// Resolve symlink installations too; never search PATH or the working directory.
	executable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(executable)
	raw, err := os.ReadFile(filepath.Join(directory, "release.json"))
	if err != nil {
		return nil, err
	}
	var metadata releaseMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("invalid release.json: %w", err)
	}
	if metadata.Version != version || source == "" || metadata.SourceCommit != source || metadata.Platform != "darwin/"+arch {
		return nil, fmt.Errorf("release.json does not match this CLI's version, source revision or platform")
	}
	host, err := os.ReadFile(executable)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(host)) != metadata.BinarySHA256 {
		return nil, fmt.Errorf("CLI checksum differs from release.json")
	}
	companion := metadata.LinuxCompanion
	if companion == nil || companion.Filename != companionName || companion.Platform != "linux/"+arch {
		return nil, fmt.Errorf("release.json requires %s for linux/%s", companionName, arch)
	}
	path := filepath.Join(directory, companionName)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, fmt.Errorf("%s must be a regular executable file", companionName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != companion.SHA256 {
		return nil, fmt.Errorf("%s checksum differs from release.json", companionName)
	}
	image, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s is not a Linux ELF executable: %w", companionName, err)
	}
	defer image.Close()
	machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if machine == 0 || image.Machine != machine || image.Class != elf.ELFCLASS64 || image.Type != elf.ET_EXEC {
		return nil, fmt.Errorf("%s is not a linux/%s executable", companionName, arch)
	}
	identity, err := buildinfo.Read(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read companion Go build identity: %w", err)
	}
	settings := make(map[string]string)
	for _, setting := range identity.Settings {
		settings[setting.Key] = setting.Value
	}
	// The manifest binds both hashes to this CLI's embedded version/revision.
	// Go omits linker flags from build info under -trimpath; do not infer a
	// release identity from those flags. CI also executes the packaged companion.
	if settings["GOOS"] != "linux" || settings["GOARCH"] != arch || settings["CGO_ENABLED"] != "0" {
		return nil, fmt.Errorf("companion build identity differs (platform or static build)")
	}
	return data, nil
}
