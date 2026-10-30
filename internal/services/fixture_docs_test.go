package services

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentedHTTPFixture(t *testing.T) {
	doc, err := os.ReadFile("../../docs/environment-blueprint.md")
	if err != nil {
		t.Fatal(err)
	}
	_, example, ok := strings.Cut(string(doc), "Fixture asset syntax:\n\n```yaml\n")
	if !ok {
		t.Fatal("documented HTTP fixture example not found")
	}
	fixture, _, ok := strings.Cut(example, "\n```")
	if !ok {
		t.Fatal("unterminated fixture example")
	}
	root := t.TempDir()
	path := filepath.Join(root, "fixture.yaml")
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	set, err := LoadFixtureSet(path)
	if err != nil {
		t.Fatalf("documented fixture rejected: %v", err)
	}
	request := httptest.NewRequest("GET", "/users?email=alice%40example.test", nil)
	request.Header.Set("Accept", "application/json")
	response := httptest.NewRecorder()
	NewFixtureHandler("directory", set, root, nil).ServeHTTP(response, request)
	var body map[string]string
	if response.Code != 200 || response.Header().Get("X-Fixture") != "find-alice" ||
		json.Unmarshal(response.Body.Bytes(), &body) != nil || body["id"] != "user-1" || body["email"] != "alice@example.test" {
		t.Fatalf("documented fixture response: %d %s", response.Code, response.Body)
	}
}
