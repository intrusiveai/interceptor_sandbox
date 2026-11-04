package bridge

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUnixSocketPaths(t *testing.T) {
	for _, long := range []bool{false, true} {
		name := "short"
		if long {
			name = "long"
		}
		t.Run(name, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "i-sock-")
			if err != nil {
				t.Fatal(err)
			}
			cleanupRoot := root
			t.Cleanup(func() { _ = os.RemoveAll(cleanupRoot) })
			if long {
				root = filepath.Join(root, strings.Repeat("nested-", 25), "sockets")
			}
			path := filepath.Join(root, "https-seam.sock")
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "connected") })
			server, listener, err := ListenUnix(path, handler)
			if long && runtime.GOOS != "linux" {
				if err == nil || !strings.Contains(err.Error(), "--bridge-transport spool") {
					t.Fatalf("expected actionable long path error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			defer listener.Close()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
				t.Fatalf("socket mode = %v", info.Mode())
			}
			go server.Serve(listener)
			transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return DialUnix(ctx, path) }}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			response, err := client.Get("http://unix/health")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(body) != "connected" {
				t.Fatalf("response = %q, %v", body, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if conn, err := DialUnix(ctx, path); err == nil {
				conn.Close()
				t.Fatal("canceled dial succeeded")
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("socket not removed: %v", err)
			}
		})
	}
}

func TestUnixSocketDoesNotReplaceRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "socket")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, listener, err := ListenUnix(path, http.NotFoundHandler()); err == nil {
		listener.Close()
		t.Fatal("replaced regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatalf("file changed: %q, %v", data, err)
	}
}
