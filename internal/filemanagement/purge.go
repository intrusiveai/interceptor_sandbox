package filemanagement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

type PurgeSelection struct {
	SessionID, CampaignID string
	All                   bool
}

func Purge(ctx context.Context, root string, selection PurgeSelection, docker ociruntime.Docker, output io.Writer) error {
	count := 0
	if selection.SessionID != "" {
		count++
	}
	if selection.CampaignID != "" {
		count++
	}
	if selection.All {
		count++
	}
	if count != 1 {
		return errors.New("purge requires exactly one of --session, --campaign, or --all")
	}
	lock, err := Lock(root, "maintenance", true)
	if err != nil {
		return err
	}
	defer lock.Close()
	items, err := session.List(root)
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	var writerLocks []*os.File
	defer func() {
		for _, f := range writerLocks {
			f.Close()
		}
	}()
	for _, m := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !selection.All && m.ID != selection.SessionID && (selection.CampaignID == "" || m.CampaignID != selection.CampaignID) {
			continue
		}
		f, err := Lock(root, "writers-"+m.ID, true)
		if err != nil {
			return err
		}
		writerLocks = append(writerLocks, f)
		bound, containerID, err := docker.ForSession(ctx, root, m.ID)
		if err != nil {
			return err
		}
		present, err := bound.ContainerPresent(ctx, containerID)
		if err != nil {
			return err
		}
		if present {
			running, err := bound.Running(ctx, containerID)
			if err != nil {
				return err
			}
			if running {
				return fmt.Errorf("refusing to purge active session %s", m.ID)
			}
		}
		selected[m.ID] = true
	}
	// Audit writers also take maintenance shared access; the exclusive lock above
	// covers --all's installation audit deletion as well as all session operations.
	if len(selected) == 0 {
		fmt.Fprintln(output, "Selected session/campaign groups are already absent")
	}
	// Validate inventory edits before touching any evidence. Persist them after
	// deletion; on partial failure keep metadata until last so retry can rediscover it.
	ledgers, err := purgedLedgers(root, selected)
	if err != nil {
		return err
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer parent.Close()
	for _, m := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !selected[m.ID] {
			continue
		}
		relative := filepath.Join("sessions", m.ID)
		group, err := parent.OpenRoot(relative)
		if err != nil {
			return err
		}
		entries, err := group.Open(".")
		if err != nil {
			group.Close()
			return err
		}
		for {
			names, readErr := entries.Readdirnames(128)
			for _, name := range names {
				if err := ctx.Err(); err != nil {
					entries.Close()
					group.Close()
					return err
				}
				if name != "session.json" {
					if err := group.RemoveAll(name); err != nil {
						entries.Close()
						group.Close()
						return fmt.Errorf("partial purge %s (retry): %w", m.ID, err)
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				entries.Close()
				group.Close()
				return readErr
			}
		}
		entries.Close()
		group.Close()
		if err := parent.RemoveAll(relative); err != nil {
			return fmt.Errorf("partial purge %s (retry): %w", m.ID, err)
		}
		for _, path := range []string{ociruntime.BindingPath(root, m.ID), ociruntime.BindingPath(root, m.ID) + ".exited"} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		fmt.Fprintf(output, "Purged session %s: %s\n", m.ID, filepath.Join(root, relative))
	}
	for path, data := range ledgers {
		if data == nil {
			err = os.Remove(path)
			if os.IsNotExist(err) {
				err = nil
			}
		} else {
			err = fsutil.WriteFile(path, data, 0600)
		}
		if err != nil {
			return fmt.Errorf("purge inventory update incomplete (retry): %w", err)
		}
	}
	if selection.All {
		// Also retire managed evidence directories left by earlier development layouts.
		for _, name := range []string{"audit", "blobs", "exports"} {
			if err := parent.RemoveAll(name); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(output, "Purged resources can no longer generate reports, serve feedback, or restore checkpoints. External exports and installation configuration/images are preserved.")
	return nil
}

// Ledger fields are retained verbatim except for references to removed/missing
// session groups. The running service cannot own this ledger under our lock.
func purgedLedgers(root string, selected map[string]bool) (map[string][]byte, error) {
	result := map[string][]byte{}
	directory := filepath.Join(root, "instances")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("instance ledger is a symlink")
		}
		path := filepath.Join(directory, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var state map[string]json.RawMessage
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, err
		}
		var sessions map[string]json.RawMessage
		if err := json.Unmarshal(state["sessions"], &sessions); err != nil {
			return nil, err
		}
		removed := map[string]bool{}
		for id := range sessions {
			_, _, loadErr := session.Load(root, id)
			if selected[id] || errors.Is(loadErr, os.ErrNotExist) {
				removed[id] = true
				delete(sessions, id)
			} else if loadErr != nil {
				return nil, fmt.Errorf("inventory session %s: %w", id, loadErr)
			}
		}
		if len(sessions) == 0 {
			result[path] = nil
			continue
		}
		var records map[string]json.RawMessage
		if err := json.Unmarshal(state["records"], &records); err != nil {
			return nil, err
		}
		for id, raw := range records {
			var record struct {
				Request struct {
					SessionID       string `json:"session_id"`
					SourceSessionID string `json:"source_session_id"`
				}
				Response struct{ Body json.RawMessage }
			}
			if err := json.Unmarshal(raw, &record); err != nil {
				return nil, err
			}
			var response struct {
				Binding struct {
					SessionID string `json:"session_id"`
				}
			}
			_ = json.Unmarshal(record.Response.Body, &response)
			if removed[record.Request.SessionID] || removed[record.Request.SourceSessionID] || removed[response.Binding.SessionID] {
				delete(records, id)
			}
		}
		for _, field := range []string{"active"} {
			var binding struct {
				SessionID string `json:"session_id"`
			}
			_ = json.Unmarshal(state[field], &binding)
			if removed[binding.SessionID] {
				state[field] = json.RawMessage(`{}`)
				state["closed"] = json.RawMessage(`true`)
				state["phase"] = json.RawMessage(`"stopped"`)
			}
		}
		var transition string
		_ = json.Unmarshal(state["transition_session_id"], &transition)
		if removed[transition] {
			delete(state, "transition_session_id")
		}
		state["sessions"], _ = json.Marshal(sessions)
		state["records"], _ = json.Marshal(records)
		result[path], err = json.MarshalIndent(state, "", "  ")
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
