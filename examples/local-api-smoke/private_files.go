package main

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type privateOutput struct {
	ProcessCaps []struct {
		UID       int    `json:"uid"`
		Effective uint64 `json:"effective"`
		Permitted uint64 `json:"permitted"`
	} `json:"process_caps"`
	Content *string `json:"content"`
	Seed    string  `json:"seed"`
	UID     int     `json:"uid"`
	GID     int     `json:"gid"`
	Entries map[string]struct {
		UID  int    `json:"uid"`
		GID  int    `json:"gid"`
		Mode uint32 `json:"mode"`
	} `json:"entries"`
}

func privateTurn(b localapi.Binding, command string) privateOutput {
	payload := []byte(`"private snapshot test"`)
	descriptor := adaptive.ArtifactDescriptor{Digest: digest.Bytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/json", Canonicalization: "raw"}
	success(operation(b, "artifact.register", map[string]any{"descriptor": descriptor, "content": payload}, nil))
	id := fmt.Sprintf("private-%d", sequence)
	a := adaptive.AttemptContext{APIVersion: adaptive.AttemptContextVersion, CampaignID: campaign, ThreadID: id, AttemptID: id, Generation: 1, AttemptIndex: uint64(sequence), Payload: descriptor, Generator: adaptive.Generator{Kind: "custom-harness"}, FeedbackProfile: "black-box", CreatedAt: time.Now().UTC()}
	success(operation(b, "attempt.register", a, &a))
	raw := success(operation(b, "application.invoke", map[string]any{"operation": "probe", "input": []byte(command), "media_type": "text/plain", "attempt_id": a.AttemptID, "payload_digest": a.Payload.Digest}, &a))
	var turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	must(json.Unmarshal(raw, &turn))
	require(turn.Status == "complete", string(raw))
	var view adaptive.ObservationView
	must(json.Unmarshal(success(operation(b, "observation.read", adaptive.ObservationRequest{TurnID: turn.ID}, &a)), &view))
	for _, entry := range view.Entries {
		if entry.Kind == "target_output" && entry.Availability == "available" {
			var chunk adaptive.FeedbackChunk
			must(json.Unmarshal(success(operation(b, "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: view.ReceiptID, EntryID: entry.ID, MaxBytes: adaptive.MaxFeedbackChunk}, &a)), &chunk))
			var result privateOutput
			must(json.Unmarshal(chunk.Content, &result))
			return result
		}
	}
	panic("private-file target output missing")
}

func qualifyPrivateFiles(first localapi.Binding, output string, limit int64, dataRoot string) {
	capture := func(b localapi.Binding) snapshot.Checkpoint {
		var cp snapshot.Checkpoint
		must(json.Unmarshal(success(operation(b, "snapshot.create", map[string]string{"label": "private-files"}, nil)), &cp))
		return cp
	}
	restore := func(b localapi.Binding, cp snapshot.Checkpoint) localapi.Binding {
		var r struct {
			Binding localapi.Binding `json:"binding"`
		}
		must(json.Unmarshal(success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: b.WorkerInstanceID, RunRevision: b.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: b.SessionID, SourceSessionID: cp.SourceSessionID, OperationID: "restore-" + cp.ID, Operation: "snapshot.restore", CheckpointID: cp.ID})), &r))
		return r.Binding
	}
	check := func(value privateOutput, expected string) {
		require(value.UID == 20000 && value.Content != nil && *value.Content == expected && value.Seed == "supervisor-seed", fmt.Sprintf("data or app identity mismatch: %+v", value))
		for path, mode := range map[string]uint32{"/state/app/evidence": 0700, "/state/app/evidence/nested": 0700, "/state/app/evidence/nested/state.txt": 0600} {
			e := value.Entries[path]
			require(e.UID == value.UID && e.GID == value.GID && e.Mode == mode, fmt.Sprintf("private ownership/mode mismatch %s: %+v", path, e))
		}
		supervisorSeen := false
		for _, caps := range value.ProcessCaps {
			if caps.UID == value.UID {
				require(caps.Effective == 0 && caps.Permitted == 0, "application retained capabilities")
			} else {
				supervisorSeen = true
				require(caps.Effective == 32 && caps.Permitted == 32, "supervisor retained bootstrap capabilities")
			}
		}
		require(supervisorSeen, "supervisor capability check missing")
		seed := value.Entries["/state/app/seed.txt"]
		require(seed.UID != value.UID && seed.GID == value.GID && seed.Mode == 0660, fmt.Sprintf("seed ownership/mode mismatch: %+v", seed))
	}
	baseline := capture(first)
	created := privateTurn(first, "create")
	check(created, "snapshot-private-content")
	if dataRoot != "" {
		require(string(readMutableFile(dataRoot, first.SessionID, "/state/app/evidence/nested/state.txt")) == "snapshot-private-content", "qualification read staging instead of live private state")
	}
	saved := capture(first)
	reset := restore(first, baseline)
	require(privateTurn(reset, "read").Content == nil, "baseline reset retained private data")
	restored := restore(reset, saved)
	read := privateTurn(restored, "read")
	check(read, "snapshot-private-content")
	updated := privateTurn(restored, "update")
	check(updated, "snapshot-private-content-updated")
	if dataRoot != "" {
		require(string(readMutableFile(dataRoot, restored.SessionID, "/state/app/evidence/nested/state.txt")) == "snapshot-private-content-updated", "qualification did not read updated restored state")
	}
	savedAgain := capture(restored)
	final := restore(restored, savedAgain)
	check(privateTurn(final, "read"), "snapshot-private-content-updated")
	success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: final.WorkerInstanceID, RunRevision: final.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: final.SessionID, OperationID: "stop-private", Operation: "session.stop"}))
	sessions := []string{first.SessionID, reset.SessionID, restored.SessionID, final.SessionID}
	for index, id := range sessions {
		evidence(output, id, limit)
		states := 0
		if index == 0 {
			states = 2
		}
		checkPrivateOracles(filepath.Join(output, id+".tar"), index, states)
	}
	data, err := json.MarshalIndent(map[string]any{"passed": true, "sessions": sessions, "created": created, "restored": read, "updated": updated, "oracles_verified": true, "target_output": "private contents, logical ownership, exact modes and file oracle verdicts survive reset/restore and recapture"}, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(output, "result.json"), data, 0600))
	fmt.Println(string(data))
}

func qualifyPrivateOracles(b localapi.Binding, output string, limit int64) {
	created := privateTurn(b, "create")
	require(created.UID == 20000 && created.Content != nil && *created.Content == "snapshot-private-content", "private-file creation failed")
	success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: b.WorkerInstanceID, RunRevision: b.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: b.SessionID, OperationID: "stop-private-oracles", Operation: "session.stop"}))
	evidence(output, b.SessionID, limit)
	checkPrivateOracles(filepath.Join(output, b.SessionID+".tar"), 0, 0)
	data, err := json.MarshalIndent(map[string]any{"passed": true, "sessions": []string{b.SessionID}, "oracles_verified": true, "snapshots_enabled": false}, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(output, "result.json"), data, 0600))
	fmt.Println(string(data))
}

func checkPrivateOracles(path string, sessionIndex, expectedStates int) {
	check := func(results []evaluation.OracleResult, present, updated bool) {
		expected := map[string]bool{"private-exists": present, "private-content": present, "private-digest": present, "private-updated": updated, "nested-missing": true}
		require(len(results) == len(expected), "missing private-file oracle verdicts")
		for _, result := range results {
			want, found := expected[result.ID]
			require(found && want == result.Fired, fmt.Sprintf("%s oracle %s: got %v, want %v", path, result.ID, result.Fired, want))
			delete(expected, result.ID)
		}
	}
	file, err := os.Open(path)
	must(err)
	defer file.Close()
	reader := tar.NewReader(file)
	finalSeen, snapshotStates := false, 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		must(err)
		switch header.Name {
		case "final-evaluation.json":
			var report evaluation.Report
			must(json.NewDecoder(reader).Decode(&report))
			// Oracle verdicts latch once fired; restoring preserves those verdicts.
			check(report.Oracles, sessionIndex != 1, sessionIndex >= 2)
			finalSeen = true
		case "state.jsonl":
			if expectedStates == 0 {
				continue
			}
			decoder := json.NewDecoder(reader)
			for {
				var tx statejournal.Transaction
				if err := decoder.Decode(&tx); err == io.EOF {
					break
				} else {
					must(err)
				}
				for _, op := range tx.Operations {
					if op.Kind == "oracle_state_set" {
						var results []evaluation.OracleResult
						must(json.Unmarshal(op.Value, &results))
						check(results, snapshotStates > 0, false)
						snapshotStates++
					}
				}
			}
		}
	}
	require(finalSeen, "final private-file oracle evaluation missing")
	require(snapshotStates == expectedStates, "baseline and private checkpoint verdicts missing")
}
