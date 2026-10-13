// Package catalog validates and merges product release catalogs independently of the website.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var (
	releaseVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?$`)
	platformPattern       = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9][a-z0-9._-]*$`)
	sha256Pattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)

	Products = []Product{
		{ID: "operator_sandbox", Kind: "github_release", Repository: "operator_sandbox"},
		{ID: "attack_harness", Kind: "oci_image", Repository: "attack_harness"},
		{ID: "interceptor_sandbox", Kind: "github_release", Repository: "interceptor_sandbox"},
	}
)

type Product struct {
	ID         string
	Kind       string
	Repository string
}

type Index struct {
	SchemaVersion int     `json:"schema_version"`
	Product       string  `json:"product"`
	Kind          string  `json:"kind"`
	Releases      []Entry `json:"releases"`
}

type Entry struct {
	Version   string     `json:"version"`
	Latest    bool       `json:"latest"`
	Tags      []string   `json:"tags,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Image     string     `json:"image,omitempty"`
	Digest    string     `json:"digest,omitempty"`
}

type Artifact struct {
	Platform string `json:"platform"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
}

func RequireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func Validate(index Index, product Product) error {
	if index.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1, got %d", index.SchemaVersion)
	}
	if index.Product != product.ID {
		return fmt.Errorf("product must be %q", product.ID)
	}
	if index.Kind != product.Kind {
		return fmt.Errorf("kind must be %q", product.Kind)
	}
	if index.Releases == nil {
		return errors.New("releases must be an array")
	}
	versions := make(map[string]struct{}, len(index.Releases))
	for position, release := range index.Releases {
		if !releaseVersionPattern.MatchString(release.Version) {
			return fmt.Errorf("releases[%d].version must be a v-prefixed or unprefixed semantic version", position)
		}
		if _, exists := versions[release.Version]; exists {
			return fmt.Errorf("duplicate release version %q", release.Version)
		}
		versions[release.Version] = struct{}{}
		tags := make(map[string]struct{}, len(release.Tags))
		for _, tag := range release.Tags {
			if tag == "" || strings.TrimSpace(tag) != tag {
				return fmt.Errorf("releases[%d].tags must contain nonempty labels without surrounding whitespace", position)
			}
			if _, exists := tags[tag]; exists {
				return fmt.Errorf("releases[%d].tags contains duplicate label %q", position, tag)
			}
			tags[tag] = struct{}{}
		}
		if release.Latest != (position == 0) {
			return fmt.Errorf("releases[%d].latest must be %t", position, position == 0)
		}
		if product.Kind == "github_release" {
			if err := validateGitHubRelease(release, product, position); err != nil {
				return err
			}
			continue
		}
		if err := validateOCIRelease(release, product, position); err != nil {
			return err
		}
	}
	return nil
}

func validateGitHubRelease(release Entry, product Product, position int) error {
	if release.Image != "" || release.Digest != "" {
		return fmt.Errorf("releases[%d] must not define image or digest for a GitHub release", position)
	}
	if len(release.Artifacts) == 0 {
		return fmt.Errorf("releases[%d].artifacts must contain at least one downloadable architecture", position)
	}
	platforms := make(map[string]struct{}, len(release.Artifacts))
	for artifactPosition, artifact := range release.Artifacts {
		prefix := fmt.Sprintf("releases[%d].artifacts[%d]", position, artifactPosition)
		if !platformPattern.MatchString(artifact.Platform) {
			return fmt.Errorf("%s.platform must use os/architecture form", prefix)
		}
		if _, exists := platforms[artifact.Platform]; exists {
			return fmt.Errorf("releases[%d] contains duplicate platform %q", position, artifact.Platform)
		}
		platforms[artifact.Platform] = struct{}{}
		if artifact.Filename == "" || path.Base(artifact.Filename) != artifact.Filename {
			return fmt.Errorf("%s.filename must be a file name without a path", prefix)
		}
		if !sha256Pattern.MatchString(artifact.SHA256) {
			return fmt.Errorf("%s.sha256 must be 64 lowercase hexadecimal characters", prefix)
		}
		if err := validateGitHubAssetURL(artifact.URL, product.Repository); err != nil {
			return fmt.Errorf("%s.url: %w", prefix, err)
		}
	}
	return nil
}

func validateGitHubAssetURL(rawURL, repository string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	wantPrefix := "/intrusiveai/" + repository + "/releases/download/"
	if parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || !strings.HasPrefix(parsed.EscapedPath(), wantPrefix) {
		return fmt.Errorf("must be a direct https://github.com%s... release asset URL", wantPrefix)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must not contain a query or fragment")
	}
	return nil
}

func validateOCIRelease(release Entry, product Product, position int) error {
	if len(release.Artifacts) != 0 {
		return fmt.Errorf("releases[%d] must not define artifacts for an OCI image", position)
	}
	wantImage := "ghcr.io/intrusiveai/" + product.Repository + ":" + release.Version
	if release.Image != wantImage {
		return fmt.Errorf("releases[%d].image must be %q", position, wantImage)
	}
	if release.Digest != "" && (!strings.HasPrefix(release.Digest, "sha256:") || !sha256Pattern.MatchString(strings.TrimPrefix(release.Digest, "sha256:"))) {
		return fmt.Errorf("releases[%d].digest must be empty or a sha256 digest", position)
	}
	return nil
}
