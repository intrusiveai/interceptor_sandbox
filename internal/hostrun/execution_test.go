package hostrun

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/appimage"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/testdocker"
)

func TestExecutionFailureNotificationsAndPlannedStop(t *testing.T) {
	for _, mode := range []string{"native-limit", "unexpected-stop", "connector-failure", "planned-stop"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			docker, fake := testdocker.New(t)
			m := session.Metadata{SchemaVersion: 1, ID: "sess-3-000000000000000000000000", Phase: session.Creating, BridgeTransport: "spool", SpoolMaxBytes: 512 << 20, Limits: session.Limits{MaxBlobBytes: 1 << 20}}
			paths, events, journal, err := session.Initialize(root, m, "")
			if err != nil {
				t.Fatal(err)
			}
			events.Close()
			journal.Close()
			if _, err := blobstore.Open(paths.Blobs, 1<<20); err != nil {
				t.Fatal(err)
			}
			deadline, finish := context.WithTimeout(context.Background(), 15*time.Second)
			defer finish()
			ctx, cancel := context.WithCancelCause(deadline)
			defer cancel(PlannedStop)
			// The private HTTP peer supplies readiness/stop only; Docker is a CLI double.
			stubCtx, stopStub := context.WithCancel(deadline)
			stubDone := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/stop" {
					latest, _, err := session.Load(root, m.ID)
					if err == nil {
						latest.Phase = session.Stopped
						_ = session.Save(paths.Metadata, latest)
					}
				}
				w.Write([]byte(`{}`))
			})
			go func() {
				defer close(stubDone)
				_ = bridge.ServeHTTPSpool(stubCtx, bridge.HTTPSpoolServerConfig{Root: filepath.Join(paths.Sockets, "http-spool"), Control: handler, Application: handler, MaxBytes: 1 << 20})
			}()
			defer func() { stopStub(); <-stubDone }()
			ready := make(chan struct{}, 1)
			fault := make(chan error, 1)
			done := make(chan error, 1)
			releaseFault := make(chan struct{})
			defer func() {
				select {
				case <-releaseFault:
				default:
					close(releaseFault)
				}
			}()
			go func() {
				_, err := runPrepared(ctx, Config{Docker: docker, BridgeTransport: "spool", InternalOnly: true, ProviderURL: "http://127.0.0.1:9", EnvironmentDirectory: root, OnReady: func(session.Metadata) error { ready <- struct{}{}; return nil }, OnFault: func(_ string, err error) {
					fault <- err
					if mode == "native-limit" {
						<-releaseFault
					}
				}}, root, &environment.Blueprint{}, appimage.Lock{WrapperImage: "test:local"}, m, paths, nil)
				done <- err
			}()
			for {
				if _, err := os.Stat(filepath.Join(fake, testdocker.CreatedID+".running")); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatal("startup failed", err)
				case <-deadline.Done():
					t.Fatal("start timeout")
				case <-time.After(10 * time.Millisecond):
				}
			}
			m, _, err = session.Load(root, m.ID)
			if err != nil {
				t.Fatal(err)
			}
			m.Phase = session.Running
			if err := session.Save(paths.Metadata, m); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ready:
			case err := <-done:
				t.Fatal(err)
			case <-deadline.Done():
				t.Fatal("readiness timeout")
			}
			expected := ""
			switch mode {
			case "native-limit":
				m.Phase = session.Error
				m.TerminationReason = "WALL_TIME_LIMIT"
				expected = "WALL_TIME_LIMIT"
				if err := session.Save(paths.Metadata, m); err != nil {
					t.Fatal(err)
				}
			case "unexpected-stop":
				m.Phase = session.Stopped
				expected = "CONTAINER_INTERRUPTED"
				if err := session.Save(paths.Metadata, m); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(fake, testdocker.CreatedID+".running")); err != nil {
					t.Fatal(err)
				}
			case "connector-failure":
				expected = "MODEL_CONNECTOR_FAILED"
				if err := os.RemoveAll(filepath.Join(paths.Sockets, "model-spool", "requests")); err != nil {
					t.Fatal(err)
				}
			case "planned-stop":
				cancel(PlannedStop)
			}
			if expected != "" {
				select {
				case err := <-fault:
					if !strings.Contains(err.Error(), expected) {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("failure notification missing")
				}
			}
			if mode == "native-limit" {
				// Simulate API persistence being stuck: it must not prevent Docker kill.
				limit := time.Now().Add(3 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(fake, testdocker.CreatedID+".running")); os.IsNotExist(err) {
						break
					}
					if time.Now().After(limit) {
						t.Fatal("termination waited for fault callback")
					}
					time.Sleep(10 * time.Millisecond)
				}
				close(releaseFault)
			}
			select {
			case err := <-done:
				if expected != "" && (err == nil || !strings.Contains(err.Error(), expected)) {
					t.Fatal("lost terminal reason", err)
				}
				if expected == "" && err != nil {
					t.Fatal("planned stop failed", err)
				}
			case <-deadline.Done():
				t.Fatal("cleanup timeout")
			}
			select {
			case err := <-fault:
				t.Fatal("duplicate or planned fault", err)
			default:
			}
		})
	}
}
