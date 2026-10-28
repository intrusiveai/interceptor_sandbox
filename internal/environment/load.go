package environment

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func Load(directory string) (*Blueprint, error) {
	path := filepath.Join(directory, BlueprintFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var blueprint Blueprint
	if err := decoder.Decode(&blueprint); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode %s: multiple YAML documents are not allowed", path)
		}
		return nil, fmt.Errorf("decode trailing %s data: %w", path, err)
	}
	blueprint.Limits.ApplyDefaults()
	blueprint.Adaptive.ApplyDefaults()
	if blueprint.Network.DNS.Enabled {
		if blueprint.Network.DNS.Listen == "" {
			blueprint.Network.DNS.Listen = "127.0.0.1:53"
		}
		if blueprint.Network.DNS.UnknownPolicy == "" {
			blueprint.Network.DNS.UnknownPolicy = "nxdomain"
		}
		if blueprint.Network.DNS.MaxQueries == 0 {
			blueprint.Network.DNS.MaxQueries = 10_000
		}
	}
	if blueprint.Network.HTTP.Enabled && blueprint.Network.HTTP.Listen == "" {
		blueprint.Network.HTTP.Listen = "127.0.0.1:80"
	}
	if blueprint.Network.TLS.Enabled {
		if blueprint.Network.TLS.Listen == "" {
			blueprint.Network.TLS.Listen = "127.0.0.1:443"
		}
		if blueprint.Network.TLS.TrustMode == "" {
			blueprint.Network.TLS.TrustMode = "auto"
		}
	}
	for i := range blueprint.Services {
		if c := blueprint.Services[i].PostgreSQL; c != nil {
			c.Defaults()
		}
	}
	for index := range blueprint.AttackerServices {
		if blueprint.AttackerServices[index].Response.Mode == "" {
			blueprint.AttackerServices[index].Response.Mode = "static"
		}
	}
	return &blueprint, nil
}
