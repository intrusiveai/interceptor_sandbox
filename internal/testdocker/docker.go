// Package testdocker provides a deterministic Docker CLI double for host tests.
package testdocker

import (
	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"os"
	"path/filepath"
	"testing"
)

func New(t *testing.T) (ociruntime.Docker, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("INTERCEPTOR_TEST_DOCKER", root)
	binary := filepath.Join(root, "docker")
	script := `#!/bin/sh
set -eu
root="$INTERCEPTOR_TEST_DOCKER"
echo "$*" >> "$root/calls"
if test -f "$root/unavailable"; then exit 1; fi
case "$1" in
run)
 shift
 while test "$1" != "--name"; do shift; done
 name="$2"
 touch "$root/$name.present" "$root/$name.running"
 echo "$name";;
ps)
 for arg in "$@"; do
 case "$arg" in name=*) name="${arg#name=^/}"; name="${name%$}"; test ! -f "$root/$name.present" || echo "$name";; esac
 done;;
inspect)
 for name in "$@"; do :; done
 if test -f "$root/$name.running"; then echo true; else echo false; fi;;
kill|stop)
 for name in "$@"; do :; done
 rm -f "$root/$name.running";;
rm)
 rm -f "$root/$2.present";;
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
