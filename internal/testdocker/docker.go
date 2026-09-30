// Package testdocker provides a deterministic Docker CLI double for host tests.
package testdocker

import (
	"crypto/sha256"
	"fmt"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"os"
	"path/filepath"
	"testing"
)

func New(t *testing.T) (ociruntime.Docker, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("INTERCEPTOR_TEST_DOCKER", root)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	binary := filepath.Join(root, "docker")
	script := `#!/bin/sh
set -eu
root="$INTERCEPTOR_TEST_DOCKER"
echo "$*" >> "$root/calls"
if test -f "$root/unavailable"; then exit 1; fi
if test "$1" = "--host"; then
 test -z "${DOCKER_HOST:-}${DOCKER_CONTEXT:-}${DOCKER_TLS_VERIFY:-}"
 test "$2" = "unix:///test/docker.sock" || exit 1
 shift 2
fi
case "$1" in
context) echo unix:///test/docker.sock;;
info) if test -f "$root/daemon-id"; then cat "$root/daemon-id"; else echo test-daemon; fi;;
create)
 id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
 name=""; session=""
 while test "$#" -gt 0; do
 case "$1" in --name) name="$2"; shift;; --label) session="${2#*=}"; shift;; esac
 shift
 done
 echo "$name" > "$root/$id.name"
 echo "$session" > "$root/$id.session"
 touch "$root/$id.present"
 echo "$id";;
start) touch "$root/$2.running";;
ps)
 for arg in "$@"; do
 case "$arg" in
 id=*) id="${arg#id=}"; test ! -f "$root/$id.present" || echo "$id";;
 label=*)
  label="${arg#label=}"; session="${label#*=}"
  for path in "$root/"*.present; do
   test -f "$path" || continue
   id="${path##*/}"; id="${id%.present}"
   if test "$label" = "$session"; then
    test ! -f "$root/$id.running" || echo "$id"
   elif test "$(cat "$root/$id.session")" = "$session"; then echo "$id"; fi
  done;;
 esac
 done;;
container)
 for id in "$@"; do :; done
 test -f "$root/$id.present" || exit 1
 running=false; test ! -f "$root/$id.running" || running=true
 session="$(cat "$root/$id.session")"
 echo "{\"Id\":\"$id\",\"Config\":{\"Labels\":{\"ai.intrusive.interceptor.session\":\"$session\"}},\"State\":{\"Running\":$running}}";;
kill|stop)
 for id in "$@"; do :; done
 rm -f "$root/$id.running";;
rm) rm -f "$root/$2.present";;
events) exec sleep 180;;
logs) :;;
*) exit 1;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return ociruntime.Docker{Binary: binary}, root
}

const CreatedID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Retired installs a host binding and exit receipt for a past test session.
func Retired(t *testing.T, root, sessionID string) string {
	t.Helper()
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(sessionID)))
	b := ociruntime.Binding{Endpoint: "unix:///test/docker.sock", DaemonID: "test-daemon", SessionID: sessionID, ContainerID: id, CreateAttempted: true}
	path := ociruntime.BindingPath(root, sessionID)
	if err := fsutil.WriteJSON(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteJSON(path+".exited", map[string]bool{"confirmed": true}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("INTERCEPTOR_TEST_DOCKER"), id+".session"), []byte(sessionID), 0600); err != nil {
		t.Fatal(err)
	}
	return id
}
