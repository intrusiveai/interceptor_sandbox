package environment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

func Seal(directory string, now time.Time) (*Lock, *ValidationReport, error) {
	report, err := Validate(directory)
	if err != nil {
		return nil, report, err
	}
	lock, err := lockFor(report, now)
	if err != nil {
		return nil, report, err
	}
	path := filepath.Join(directory, LockFile)
	if err := fsutil.WriteJSON(path, lock, 0o640); err != nil {
		return nil, report, err
	}
	return lock, report, nil
}

func Verify(directory string) (*Lock, *ValidationReport, error) {
	report, err := Validate(directory)
	if err != nil {
		return nil, report, err
	}
	path := filepath.Join(directory, LockFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, report, fmt.Errorf("read Sealed Environment lock: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var existing Lock
	if err := decoder.Decode(&existing); err != nil {
		return nil, report, fmt.Errorf("decode Sealed Environment lock: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, report, fmt.Errorf("Sealed Environment lock contains trailing JSON")
	}
	expected, err := lockFor(report, existing.SealedAt)
	if err != nil {
		return nil, report, err
	}
	if existing.EnvironmentDigest != expected.EnvironmentDigest {
		return nil, report, fmt.Errorf("stale Sealed Environment lock: have %s, computed %s", existing.EnvironmentDigest, expected.EnvironmentDigest)
	}
	if existing.SchemaVersion != expected.SchemaVersion || existing.Name != expected.Name ||
		existing.Version != expected.Version || existing.ApplicationDigest != expected.ApplicationDigest {
		return nil, report, fmt.Errorf("Sealed Environment lock metadata does not match Blueprint")
	}
	if !assetsEqual(existing.Assets, expected.Assets) {
		return nil, report, fmt.Errorf("Sealed Environment asset manifest does not match Blueprint")
	}
	return &existing, report, nil
}

func lockFor(report *ValidationReport, sealedAt time.Time) (*Lock, error) {
	applicationDigest, err := ApplicationDigest(report.Blueprint.Application.Image)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(struct {
		SchemaVersion     int        `json:"schema_version"`
		Blueprint         *Blueprint `json:"blueprint"`
		Assets            []Asset    `json:"assets"`
		ApplicationDigest string     `json:"application_digest"`
	}{1, report.Blueprint, report.Assets, applicationDigest})
	if err != nil {
		return nil, fmt.Errorf("canonicalize Environment Blueprint: %w", err)
	}
	return &Lock{
		SchemaVersion: 1, Name: report.Blueprint.Metadata.Name,
		Version: report.Blueprint.Metadata.Version, ApplicationDigest: applicationDigest,
		Assets: report.Assets, EnvironmentDigest: digest.Bytes(canonical), SealedAt: sealedAt.UTC(),
	}, nil
}

func assetsEqual(left, right []Asset) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
