// local-api-smoke is a destructive test client for a fresh reference environment.
// It attaches one campaign, exercises native requests, restores, stops and exports.
package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	evidencepolicy "github.com/intrusive-ai/interceptor-sandbox/internal/evidence"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

var client = &http.Client{Timeout: 6 * time.Minute}

const campaign = "local-api-smoke"

var sequence int

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func require(ok bool, message string) {
	if !ok {
		panic(message)
	}
}
func post(path string, input any) adaptive.GatewayResponse {
	raw, err := json.Marshal(input)
	must(err)
	return postRaw(path, raw)
}
func postRaw(path string, raw []byte) adaptive.GatewayResponse {
	response, err := client.Post("http://127.0.0.1:8080"+path, "application/json", bytes.NewReader(raw))
	must(err)
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 5<<20))
	must(err)
	var result adaptive.GatewayResponse
	if err = json.Unmarshal(data, &result); err != nil {
		panic(fmt.Sprintf("%s HTTP %d: %s: %v", path, response.StatusCode, data, err))
	}
	return result
}
func success(result adaptive.GatewayResponse) json.RawMessage {
	require(result.Status >= 200 && result.Status < 300, fmt.Sprintf("operation failed: %d %s", result.Status, result.Body))
	return result.Body
}
func operation(b localapi.Binding, name string, body any, attempt *adaptive.AttemptContext) adaptive.GatewayResponse {
	sequence++
	id := fmt.Sprintf("smoke-%d", sequence)
	raw, err := json.Marshal(body)
	must(err)
	revision := uint64(0)
	if name != "session.status" {
		status := operation(b, "session.status", struct{}{}, nil)
		var state struct {
			Session session.Metadata `json:"session"`
		}
		must(json.Unmarshal(success(status), &state))
		revision = state.Session.Revision
	}
	request := adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, RequestID: id, OperationID: id, Operation: name, SessionID: b.SessionID, CampaignID: campaign, WorkerInstanceID: b.WorkerInstanceID, RunRevision: b.RunRevision, ExpectedSessionRevision: revision, BodyDigest: digest.Bytes(raw), Deadline: time.Now().Add(3 * time.Minute)}
	if attempt != nil {
		request.AttemptID = attempt.AttemptID
		request.AttemptContextDigest = adaptive.DigestAttemptContext(*attempt)
	}
	return postRaw("/v1/operations", adaptive.EncodeOperation(request, raw))
}
func attempt(b localapi.Binding) adaptive.AttemptContext {
	payload := []byte(`"smoke payload"`)
	descriptor := adaptive.ArtifactDescriptor{Digest: digest.Bytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/json", Canonicalization: "raw"}
	success(operation(b, "artifact.register", map[string]any{"descriptor": descriptor, "content": payload}, nil))
	a := adaptive.AttemptContext{APIVersion: adaptive.AttemptContextVersion, CampaignID: campaign, ThreadID: "smoke-thread", AttemptID: fmt.Sprintf("smoke-attempt-%d", b.RunRevision), Generation: b.RunRevision, AttemptIndex: b.RunRevision, Payload: descriptor, Generator: adaptive.Generator{Kind: "custom-harness"}, FeedbackProfile: "black-box", CreatedAt: time.Now().UTC()}
	if b.RunRevision > 1 {
		a.ParentAttemptID = fmt.Sprintf("smoke-attempt-%d", b.RunRevision-1)
	}
	success(operation(b, "attempt.register", a, &a))
	return a
}
func invoke(b localapi.Binding, a adaptive.AttemptContext) {
	body := map[string]any{"operation": "invoke", "input": []byte(`{"prompt":"Summarize the support ticket safely.","ticket_id":"ticket-001"}`), "media_type": "application/json", "attempt_id": a.AttemptID, "payload_digest": a.Payload.Digest}
	raw := success(operation(b, "application.invoke", body, &a))
	var turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Code   int    `json:"code"`
	}
	must(json.Unmarshal(raw, &turn))
	require(turn.Status == "complete", string(raw))
	var view adaptive.ObservationView
	must(json.Unmarshal(success(operation(b, "observation.read", adaptive.ObservationRequest{TurnID: turn.ID}, &a)), &view))
	require(len(view.Entries) > 0, "feedback has no readable target output")
	success(operation(b, "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: view.ReceiptID, EntryID: view.Entries[0].ID, MaxBytes: adaptive.MaxFeedbackChunk}, &a))
}
func counter(root, id string, want int) {
	if root == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(root, "sessions", id, "generations", "1", "runtime", "state", "app", "turns.json"))
	must(err)
	var count struct {
		Count int `json:"count"`
	}
	must(json.Unmarshal(data, &count))
	require(count.Count == want, fmt.Sprintf("%s counter=%d want=%d", id, count.Count, want))
}
func evidence(output, id string, maximum int64) {
	raw, _ := json.Marshal(map[string]string{"api_version": localapi.Version, "campaign_id": campaign, "session_id": id})
	r, err := client.Post("http://127.0.0.1:8080/v1/evidence", "application/json", bytes.NewReader(raw))
	must(err)
	defer r.Body.Close()
	require(r.StatusCode == 200, fmt.Sprintf("evidence HTTP %d", r.StatusCode))
	advertised, err := evidencepolicy.ParseLimit(r.Header.Get("X-Evidence-Max-Bytes"))
	must(err)
	maximum = min(maximum, advertised)
	require(r.ContentLength >= 0 && r.ContentLength <= maximum, "archive exceeds bound or length missing")
	expected := r.Header.Get("X-Content-SHA256")
	require(digest.Valid(expected), "archive checksum missing")
	file, err := os.CreateTemp(output, ".evidence-download-*")
	must(err)
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	hash := sha256.New()
	size, err := io.CopyN(io.MultiWriter(file, hash), r.Body, r.ContentLength)
	must(err)
	var extra [1]byte
	n, err := r.Body.Read(extra[:])
	require(n == 0 && err == io.EOF, "archive length mismatch")
	actual := fmt.Sprintf("sha256:%x", hash.Sum(nil))
	require(expected == actual, "archive checksum mismatch")
	_, err = file.Seek(0, io.SeekStart)
	must(err)
	seen := map[string]bool{}
	reader := tar.NewReader(file)
	var bundle adaptive.EvidenceBundle
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		must(err)
		require(h.Typeflag == tar.TypeReg && filepath.IsLocal(h.Name) && filepath.Clean(h.Name) == h.Name && !seen[h.Name], "unsafe or duplicate archive member")
		seen[h.Name] = true
		if h.Name == "evidence-bundle.json" {
			must(json.NewDecoder(reader).Decode(&bundle))
		}
	}
	require(seen["execution.json"] && seen["state.jsonl"] && seen["events.jsonl"] && seen["manifest.json"], "missing protected evidence members")
	require(bundle.SessionID == id && bundle.CampaignID == campaign, "evidence scope mismatch")
	require(bundle.Completeness.State == "complete", fmt.Sprintf("partial evidence: %+v gaps=%v", bundle.Completeness, bundle.CoverageGaps))
	must(file.Sync())
	must(file.Close())
	must(os.Rename(file.Name(), filepath.Join(output, id+".tar")))
	fmt.Printf("Evidence %s: %d bytes, %s, %s\n", id, size, bundle.Completeness.State, actual)
}
func main() {
	output := flag.String("output", "", "directory for evidence and result JSON")
	commandOnly := flag.Bool("command-only", false, "qualify a minimal command target's echo operation without snapshots or model calls")
	maximumEvidence := flag.Int64("evidence-max-bytes", evidencepolicy.DefaultMaxBytes, "local evidence archive acceptance ceiling")
	root := flag.String("data-root", "", "optional host data root to check reference application state rollback")
	flag.Parse()
	_, limitErr := evidencepolicy.ParseLimit(strconv.FormatInt(*maximumEvidence, 10))
	must(limitErr)
	require(*output != "", "--output is required")
	must(os.MkdirAll(*output, 0700))
	attach := localapi.AttachRequest{WorkerInstanceID: "smoke-worker", APIVersion: localapi.Version, CampaignID: campaign, AllowTargetStop: true}
	var attached struct {
		Binding          localapi.Binding `json:"binding"`
		EvidenceMaxBytes int64            `json:"evidence_max_bytes"`
	}
	must(json.Unmarshal(success(post("/v1/attach", attach)), &attached))
	_, limitErr = evidencepolicy.ParseLimit(strconv.FormatInt(attached.EvidenceMaxBytes, 10))
	must(limitErr)
	limit := min(*maximumEvidence, attached.EvidenceMaxBytes)
	first := attached.Binding
	if *commandOnly {
		qualifyCommand(first, *output, limit)
		return
	}
	success(post("/v1/attach", attach))
	other := attach
	other.CampaignID = "another-campaign"
	require(post("/v1/attach", other).Status == 409, "cross-campaign attach accepted")
	a := attempt(first)
	invoke(first, a)
	counter(*root, first.SessionID, 1)
	var checkpoint snapshot.Checkpoint
	must(json.Unmarshal(success(operation(first, "snapshot.create", map[string]string{"label": "smoke-checkpoint", "description": "Baseline before the second mutation"}, nil)), &checkpoint))
	require(checkpoint.CampaignID == campaign && checkpoint.Description == "Baseline before the second mutation", "snapshot metadata missing")
	checkInventory := func() {
		var inventory localapi.SnapshotListResponse
		must(json.Unmarshal(success(post("/v1/snapshots/list", localapi.SnapshotListRequest{APIVersion: localapi.Version, CampaignID: campaign})), &inventory))
		require(inventory.Total == 1 && len(inventory.Checkpoints) == 1 && inventory.Checkpoints[0] == checkpoint, "campaign snapshot inventory lost metadata")
		var inspected snapshot.Checkpoint
		must(json.Unmarshal(success(post("/v1/snapshots/inspect", localapi.SnapshotInspectRequest{APIVersion: localapi.Version, CampaignID: campaign, SourceSessionID: first.SessionID, CheckpointID: checkpoint.ID})), &inspected))
		require(inspected == checkpoint, "snapshot inspection lost metadata")
	}
	checkInventory()
	invoke(first, a)
	counter(*root, first.SessionID, 2)
	restore := localapi.LifecycleRequest{WorkerInstanceID: "smoke-worker", RunRevision: first.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: first.SessionID, OperationID: "restore-once", Operation: "snapshot.restore", CheckpointID: checkpoint.ID}
	original := post("/v1/lifecycle", restore)
	var restored struct {
		Binding          localapi.Binding `json:"binding"`
		EvidenceMaxBytes int64            `json:"evidence_max_bytes"`
	}
	must(json.Unmarshal(success(original), &restored))
	next := restored.Binding
	require(next.SessionID != first.SessionID && next.RunRevision == first.RunRevision+1, "restore did not create a fresh revision/session")
	require(bytes.Equal(success(post("/v1/lifecycle", restore)), original.Body), "duplicate restore changed result")
	checkInventory()
	counter(*root, next.SessionID, 1)
	require(operation(first, "snapshot.create", map[string]string{"label": "stale"}, nil).Status == 409, "old session mutation accepted")
	secondAttempt := attempt(next)
	invoke(next, secondAttempt)
	counter(*root, next.SessionID, 2)
	success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: "smoke-worker", RunRevision: next.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: next.SessionID, OperationID: "stop-final", Operation: "session.stop"}))
	checkInventory()
	evidence(*output, first.SessionID, limit)
	evidence(*output, next.SessionID, limit)
	result := map[string]any{"passed": true, "source_session_id": first.SessionID, "restored_session_id": next.SessionID, "checkpoint_id": checkpoint.ID, "run_revision": next.RunRevision, "checks": []string{"campaign binding", "idempotent attach", "native artifact/attempt/invoke/observation", "snapshot campaign/description", "inventory/inspect across restore and stop", "filesystem rollback 2 -> 1", "fresh session and revision", "idempotent restore", "stale-session rejection", "post-restore invoke", "confirmed target stop", "complete evidence for both sessions"}}
	raw, err := json.MarshalIndent(result, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(*output, "result.json"), raw, 0600))
	fmt.Println(string(raw))
}

// qualifyCommand reuses the native operation, attempt, feedback and evidence
// checks for a release-archive smoke test with /bin/cat as the application.
func qualifyCommand(binding localapi.Binding, output string, limit int64) {
	a := attempt(binding)
	message := []byte("interceptor packaged companion qualification\n")
	body := map[string]any{"operation": "echo", "input": message, "media_type": "text/plain", "attempt_id": a.AttemptID, "payload_digest": a.Payload.Digest}
	raw := success(operation(binding, "application.invoke", body, &a))
	var turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	must(json.Unmarshal(raw, &turn))
	require(turn.Status == "complete", string(raw))
	var view adaptive.ObservationView
	must(json.Unmarshal(success(operation(binding, "observation.read", adaptive.ObservationRequest{TurnID: turn.ID}, &a)), &view))
	found := false
	for _, entry := range view.Entries {
		if entry.Kind != "target_output" || entry.Availability != "available" {
			continue
		}
		var chunk adaptive.FeedbackChunk
		must(json.Unmarshal(success(operation(binding, "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: view.ReceiptID, EntryID: entry.ID, MaxBytes: adaptive.MaxFeedbackChunk}, &a)), &chunk))
		if bytes.Equal(chunk.Content, message) {
			found = true
		}
	}
	require(found, "feedback did not contain the exact command output")
	success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: binding.WorkerInstanceID, RunRevision: binding.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: binding.SessionID, OperationID: "stop-command", Operation: "session.stop"}))
	evidence(output, binding.SessionID, limit)
	raw, err := json.MarshalIndent(map[string]any{"passed": true, "session_id": binding.SessionID, "target_output": string(message), "checks": []string{"native command invocation", "exact target output", "confirmed target stop", "complete verified evidence"}}, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(output, "result.json"), raw, 0600))
	fmt.Println(string(raw))
}
