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
	return &blueprint, nil
}
