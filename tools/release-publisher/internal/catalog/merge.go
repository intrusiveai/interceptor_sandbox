package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MergeCatalogEntry updates the S3 catalog without requiring a Git change.
// New versions lead the catalog; updates preserve their existing position.
func MergeCatalogEntry(productID string, current, entryJSON []byte) ([]byte, error) {
	position := slices.IndexFunc(Products, func(p Product) bool { return p.ID == productID })
	if position < 0 {
		return nil, fmt.Errorf("unsupported catalog product %q", productID)
	}
	product := Products[position]
	index := Index{SchemaVersion: 1, Product: product.ID, Kind: product.Kind, Releases: []Entry{}}
	decode := func(raw []byte, target any) error {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return err
		}
		return RequireJSONEOF(decoder)
	}
	if len(current) > 0 {
		index = Index{}
		if err := decode(current, &index); err != nil {
			return nil, fmt.Errorf("decode current catalog: %w", err)
		}
		if err := Validate(index, product); err != nil {
			return nil, fmt.Errorf("validate current catalog: %w", err)
		}
	}
	var entry Entry
	if err := decode(entryJSON, &entry); err != nil {
		return nil, fmt.Errorf("decode catalog entry: %w", err)
	}
	// latest is a property of the merged catalog, not a caller-selected position.
	entry.Latest = true
	if product.Kind == "oci_image" && entry.Digest == "" {
		return nil, errors.New("catalog publication requires the resolved OCI digest")
	}
	if err := Validate(Index{SchemaVersion: 1, Product: product.ID, Kind: product.Kind, Releases: []Entry{entry}}, product); err != nil {
		return nil, err
	}
	position = slices.IndexFunc(index.Releases, func(existing Entry) bool { return existing.Version == entry.Version })
	if position < 0 {
		index.Releases = append([]Entry{entry}, index.Releases...)
	} else {
		existing := index.Releases[position]
		if slices.Contains(existing.Tags, "release") && (!slices.Contains(entry.Tags, "release") || !sameReleaseArtifacts(existing, entry)) {
			return nil, errors.New("cannot demote or replace the artifacts of a published release")
		}
		if slices.Contains(entry.Tags, "release") && !sameReleaseArtifacts(existing, entry) {
			return nil, errors.New("promotion must retain the candidate's artifacts")
		}
		index.Releases[position] = entry
	}
	for i := range index.Releases {
		index.Releases[i].Latest = i == 0
	}
	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// Artifact order is presentation only; promotion must retain every platform's identity.
func sameReleaseArtifacts(a, b Entry) bool {
	if a.Image != b.Image || a.Digest != b.Digest {
		return false
	}
	x, y := slices.Clone(a.Artifacts), slices.Clone(b.Artifacts)
	less := func(a, b Artifact) int { return strings.Compare(a.Platform, b.Platform) }
	slices.SortFunc(x, less)
	slices.SortFunc(y, less)
	return slices.Equal(x, y)
}
