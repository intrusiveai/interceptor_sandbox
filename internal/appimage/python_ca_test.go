package appimage

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPythonCAInstallation(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable; real package integration also runs in Docker qualification")
	}
	root := t.TempDir()
	material, err := generateTLSMaterial(time.Now(), []string{"fixture.test"})
	if err != nil {
		t.Fatal(err)
	}
	original, err := generateTLSMaterial(time.Now(), []string{"original.test"})
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(root, "sandbox.pem")
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(ca, material.ca)
	var interpreters, modules, bundles []string
	for _, name := range []string{"one", "two"} {
		venv := filepath.Join(root, name)
		if out, err := exec.Command(python, "-m", "venv", "--without-pip", venv).CombinedOutput(); err != nil {
			t.Fatalf("create fixture venv: %v: %s", err, out)
		}
		interpreter := filepath.Join(venv, "bin", "python")
		out, err := exec.Command(interpreter, "-I", "-c", `import sysconfig; print(sysconfig.get_path("purelib"))`).Output()
		if err != nil {
			t.Fatal(err)
		}
		module := filepath.Join(strings.TrimSpace(string(out)), "certifi.py")
		bundle := filepath.Join(venv, "cacert.pem")
		write(bundle, original.ca)
		literal, _ := json.Marshal(bundle)
		write(module, []byte("def where(): return "+string(literal)+"\n"))
		interpreters = append(interpreters, interpreter)
		modules = append(modules, module)
		bundles = append(bundles, bundle)
	}
	t.Run("two isolated environments preserve roots and modes", func(t *testing.T) {
		// A malicious current-directory module must not shadow the installed package.
		cwd := t.TempDir()
		write(filepath.Join(cwd, "certifi.py"), []byte("raise RuntimeError('shadowed')"))
		t.Chdir(cwd)
		for n := 0; n < 2; n++ {
			if err := InstallPythonCA(ca, "[]", interpreters); err != nil {
				t.Fatal(err)
			}
		}
		for _, bundle := range bundles {
			data, err := os.ReadFile(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(data, original.ca) || bytes.Count(data, bytes.TrimSpace(material.ca)) != 1 {
				t.Fatal("roots lost or duplicated")
			}
			info, _ := os.Stat(bundle)
			if info.Mode().Perm() != 0644 {
				t.Fatal("bundle permissions changed")
			}
		}
	})
	t.Run("shared symlink bundle", func(t *testing.T) {
		if err := os.Remove(bundles[1]); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(bundles[0], bundles[1]); err != nil {
			t.Fatal(err)
		}
		if err := InstallPythonCA(ca, "[]", interpreters); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(bundles[0])
		if bytes.Count(data, bytes.TrimSpace(material.ca)) != 1 {
			t.Fatal("shared CA duplicated")
		}
	})
	for _, tc := range []struct{ name, interpreter, mounts string }{
		{"missing interpreter", filepath.Join(root, "missing"), "[]"},
		{"nonexecutable", ca, "[]"},
		{"mount masks resolved bundle", interpreters[1], string(mustJSON(t, []string{bundles[0]}))},
		{"mount masks interpreter", interpreters[0], string(mustJSON(t, []string{filepath.Dir(interpreters[0])}))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := InstallPythonCA(ca, tc.mounts, []string{tc.interpreter}); err == nil || !strings.Contains(err.Error(), tc.interpreter) {
				t.Fatalf("missing actionable failure: %v", err)
			}
		})
	}
	t.Run("malformed bundle", func(t *testing.T) {
		write(bundles[0], []byte("invalid"))
		if err := InstallPythonCA(ca, "[]", interpreters); err == nil {
			t.Fatal("invalid bundle accepted")
		}
	})
	t.Run("missing certifi", func(t *testing.T) {
		if err := os.Remove(modules[0]); err != nil {
			t.Fatal(err)
		}
		if err := InstallPythonCA(ca, "[]", interpreters[:1]); err == nil {
			t.Fatal("missing Certifi accepted")
		}
	})
	t.Run("unwritable bundle", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write readonly files; Docker qualification tests application readability")
		}
		literal, _ := json.Marshal(bundles[0])
		write(modules[0], []byte("def where(): return "+string(literal)+"\n"))
		write(bundles[0], original.ca)
		if err := os.Chmod(bundles[0], 0444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(bundles[0], 0644) })
		if err := InstallPythonCA(ca, "[]", interpreters[:1]); err == nil {
			t.Fatal("unwritable bundle accepted")
		}
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
