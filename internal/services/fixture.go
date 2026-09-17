package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"gopkg.in/yaml.v3"
)

const maxServiceRequestBody = 1 << 20

type FixtureSet struct {
	Rules []FixtureRule `yaml:"rules" json:"rules"`
}

type FixtureRule struct {
	Delivery *delivery.Contract `yaml:"delivery,omitempty" json:"delivery,omitempty"`
	ID       string             `yaml:"id" json:"id"`
	Priority int                `yaml:"priority" json:"priority"`
	Match    FixtureMatch       `yaml:"match" json:"match"`
	Response FixtureResponse    `yaml:"response" json:"response"`
}

type FixtureMatch struct {
	Method  string            `yaml:"method" json:"method"`
	Path    string            `yaml:"path" json:"path"`
	Query   map[string]string `yaml:"query,omitempty" json:"query,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	JSON    map[string]any    `yaml:"json,omitempty" json:"json,omitempty"`
}

type FixtureResponse struct {
	Status  int               `yaml:"status" json:"status"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	JSON    any               `yaml:"json,omitempty" json:"json,omitempty"`
	Body    string            `yaml:"body,omitempty" json:"body,omitempty"`
	Asset   string            `yaml:"asset,omitempty" json:"asset,omitempty"`
}

type Coverage struct {
	Total   uint64            `json:"total"`
	Matched uint64            `json:"matched"`
	Gaps    map[string]uint64 `json:"gaps"`
}

type FixtureHandler struct {
	serviceID string
	rules     []FixtureRule
	assetRoot string
	recorder  *recording.Writer
	mu        sync.Mutex
	coverage  Coverage
}

func LoadFixtureSet(path string) (*FixtureSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fixtures: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var set FixtureSet
	if err := decoder.Decode(&set); err != nil {
		return nil, fmt.Errorf("decode fixtures: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("fixtures must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode trailing fixture content: %w", err)
	}
	if err := ValidateFixtureSet(&set); err != nil {
		return nil, err
	}
	return &set, nil
}

func ValidateFixtureSet(set *FixtureSet) error {
	if len(set.Rules) == 0 {
		return errors.New("fixture set must contain at least one rule")
	}
	ids := map[string]struct{}{}
	for index := range set.Rules {
		rule := &set.Rules[index]
		if err := delivery.Validate(rule.Delivery); err != nil {
			return fmt.Errorf("fixture %s delivery: %w", rule.ID, err)
		}
		if rule.ID == "" {
			return fmt.Errorf("fixture rule %d requires id", index)
		}
		if _, exists := ids[rule.ID]; exists {
			return fmt.Errorf("fixture rule id %q is duplicated", rule.ID)
		}
		ids[rule.ID] = struct{}{}
		rule.Match.Method = strings.ToUpper(strings.TrimSpace(rule.Match.Method))
		if rule.Match.Method == "" || !strings.HasPrefix(rule.Match.Path, "/") {
			return fmt.Errorf("fixture rule %q requires method and absolute path", rule.ID)
		}
		if rule.Response.Status < 100 || rule.Response.Status > 599 {
			return fmt.Errorf("fixture rule %q has invalid response status", rule.ID)
		}
		bodyKinds := 0
		if rule.Response.JSON != nil {
			bodyKinds++
		}
		if rule.Response.Body != "" {
			bodyKinds++
		}
		if rule.Response.Asset != "" {
			bodyKinds++
		}
		if bodyKinds > 1 {
			return fmt.Errorf("fixture rule %q must choose one response body form", rule.ID)
		}
		for previousIndex := 0; previousIndex < index; previousIndex++ {
			previous := set.Rules[previousIndex]
			if previous.Priority == rule.Priority && matchersOverlap(previous.Match, rule.Match) {
				return fmt.Errorf("fixture rules %q and %q are ambiguous at equal priority", previous.ID, rule.ID)
			}
		}
	}
	sort.SliceStable(set.Rules, func(i, j int) bool { return set.Rules[i].Priority > set.Rules[j].Priority })
	return nil
}

func matchersOverlap(left, right FixtureMatch) bool {
	if left.Method != right.Method || left.Path != right.Path || mapsConflict(left.Query, right.Query) || mapsConflict(left.Headers, right.Headers) {
		return false
	}
	return jsonObjectsCompatible(left.JSON, right.JSON)
}

func mapsConflict(left, right map[string]string) bool {
	for key, value := range left {
		if other, exists := right[key]; exists && other != value {
			return true
		}
	}
	return false
}

func jsonObjectsCompatible(left, right map[string]any) bool {
	for key, value := range left {
		other, exists := right[key]
		if !exists {
			continue
		}
		leftObject, leftIsObject := value.(map[string]any)
		rightObject, rightIsObject := other.(map[string]any)
		if leftIsObject || rightIsObject {
			if !leftIsObject || !rightIsObject || !jsonObjectsCompatible(leftObject, rightObject) {
				return false
			}
			continue
		}
		leftJSON, _ := json.Marshal(value)
		rightJSON, _ := json.Marshal(other)
		if !bytes.Equal(leftJSON, rightJSON) {
			return false
		}
	}
	return true
}

func NewFixtureHandler(serviceID string, set *FixtureSet, assetRoot string, recorder *recording.Writer) *FixtureHandler {
	return &FixtureHandler{
		serviceID: serviceID, rules: append([]FixtureRule(nil), set.Rules...), assetRoot: assetRoot, recorder: recorder,
		coverage: Coverage{Gaps: map[string]uint64{}},
	}
}

func (h *FixtureHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, err := readBounded(request.Body, maxServiceRequestBody)
	if err != nil {
		h.writeGap(writer, request, nil, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	h.mu.Lock()
	h.coverage.Total++
	h.mu.Unlock()
	for _, rule := range h.rules {
		matched, matchErr := fixtureMatches(rule.Match, request, body)
		if matchErr != nil {
			h.writeGap(writer, request, body, http.StatusBadRequest, matchErr.Error())
			return
		}
		if !matched {
			continue
		}
		h.mu.Lock()
		h.coverage.Matched++
		h.mu.Unlock()
		for name, value := range rule.Response.Headers {
			writer.Header().Set(name, value)
		}
		responseBody, contentType, responseErr := h.responseBody(rule.Response)
		if responseErr != nil {
			h.record(recording.Evaluation, "fixture_error", map[string]any{"rule": rule.ID, "error": responseErr.Error()})
			http.Error(writer, "sealed fixture is invalid", http.StatusInternalServerError)
			return
		}
		if contentType != "" && writer.Header().Get("Content-Type") == "" {
			writer.Header().Set("Content-Type", contentType)
		}
		writer.WriteHeader(rule.Response.Status)
		_, _ = writer.Write(responseBody)
		h.record(recording.Evaluation, "fixture_hit", map[string]any{"rule": rule.ID, "method": request.Method, "path": request.URL.Path, "status": rule.Response.Status})
		return
	}
	h.writeGap(writer, request, body, http.StatusNotImplemented, "no sealed fixture matched request")
}

func (h *FixtureHandler) Coverage() Coverage {
	h.mu.Lock()
	defer h.mu.Unlock()
	copy := Coverage{Total: h.coverage.Total, Matched: h.coverage.Matched, Gaps: map[string]uint64{}}
	for key, value := range h.coverage.Gaps {
		copy.Gaps[key] = value
	}
	return copy
}

func (h *FixtureHandler) writeGap(writer http.ResponseWriter, request *http.Request, _ []byte, status int, message string) {
	signature := request.Method + " " + request.URL.EscapedPath()
	if query := normalizedQuery(request.URL.Query()); query != "" {
		signature += "?" + query
	}
	h.mu.Lock()
	h.coverage.Gaps[signature]++
	h.mu.Unlock()
	h.record(recording.Evaluation, "coverage_gap", map[string]any{"service": h.serviceID, "method": request.Method, "path": request.URL.Path, "message": message})
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{"code": "coverage_gap", "message": message, "request_id": fmt.Sprintf("req-%d", serviceRequestSequence.Add(1))}})
}

func (h *FixtureHandler) responseBody(response FixtureResponse) ([]byte, string, error) {
	switch {
	case response.JSON != nil:
		data, err := json.Marshal(response.JSON)
		return data, "application/json", err
	case response.Asset != "":
		clean := filepath.Clean(response.Asset)
		if filepath.IsAbs(response.Asset) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, "", errors.New("fixture asset escapes Blueprint directory")
		}
		path := filepath.Join(h.assetRoot, clean)
		data, err := os.ReadFile(path)
		return data, http.DetectContentType(data), err
	default:
		return []byte(response.Body), "text/plain; charset=utf-8", nil
	}
}

func (h *FixtureHandler) record(channel recording.Channel, kind string, semantic any) {
	if h.recorder == nil {
		return
	}
	_, _ = h.recorder.Append(recording.Draft{Channel: channel, Layer: "fixture_service", Kind: kind, Provenance: recording.EnvironmentSealed, Semantic: semantic})
}

func fixtureMatches(match FixtureMatch, request *http.Request, body []byte) (bool, error) {
	if request.Method != match.Method || request.URL.Path != match.Path {
		return false, nil
	}
	query := request.URL.Query()
	for key, expected := range match.Query {
		if query.Get(key) != expected {
			return false, nil
		}
	}
	for key, expected := range match.Headers {
		if request.Header.Get(key) != expected {
			return false, nil
		}
	}
	if len(match.JSON) > 0 {
		var actual map[string]any
		if err := json.Unmarshal(body, &actual); err != nil {
			return false, fmt.Errorf("request body is not JSON: %w", err)
		}
		if !jsonSubset(match.JSON, actual) {
			return false, nil
		}
	}
	return true, nil
}

func jsonSubset(expected, actual map[string]any) bool {
	for key, value := range expected {
		actualValue, exists := actual[key]
		if !exists {
			return false
		}
		expectedObject, expectedIsObject := value.(map[string]any)
		actualObject, actualIsObject := actualValue.(map[string]any)
		if expectedIsObject {
			if !actualIsObject || !jsonSubset(expectedObject, actualObject) {
				return false
			}
			continue
		}
		expectedJSON, _ := json.Marshal(value)
		actualJSON, _ := json.Marshal(actualValue)
		if !bytes.Equal(expectedJSON, actualJSON) {
			return false
		}
	}
	return true
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("request body exceeds limit")
	}
	return data, nil
}

func normalizedQuery(query url.Values) string {
	result := url.Values{}
	for key, values := range query {
		result[key] = append([]string(nil), values...)
		sort.Strings(result[key])
	}
	return result.Encode()
}
