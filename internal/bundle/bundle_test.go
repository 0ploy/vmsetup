package bundle

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func build(t *testing.T, dir string) []byte {
	t.Helper()
	files := map[string]string{}
	for name, content := range map[string]string{"init": "vmsetup", "bin/zpinit": "zpinit!", "bin/zpctl": "zp"} {
		src := filepath.Join(dir, strings.ReplaceAll(name, "/", "_"))
		if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = src
	}
	out := filepath.Join(dir, "init.cpio")
	if err := Write(out, files); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLayout(t *testing.T) {
	entries, err := Read(bytes.NewReader(build(t, t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
		switch e.Name {
		case "init":
			if string(e.Data) != "vmsetup" || e.Mode != modeReg|0o755 {
				t.Errorf("init: mode %o data %q", e.Mode, e.Data)
			}
		case "dev/console":
			if e.Mode != modeChar|0o600 || e.Rdev != [2]uint32{5, 1} {
				t.Errorf("dev/console: mode %o rdev %v", e.Mode, e.Rdev)
			}
		}
	}
	want := "bin bin/zpctl bin/zpinit dev dev/console init newroot proc run"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("entries %q, want %q", got, want)
	}
}

func TestReproducible(t *testing.T) {
	a, b := build(t, t.TempDir()), build(t, t.TempDir())
	if !bytes.Equal(a, b) {
		t.Error("two builds from the same contents differ")
	}
}

// TestCpioTool cross-checks the format against GNU cpio when it is installed.
func TestCpioTool(t *testing.T) {
	if _, err := exec.LookPath("cpio"); err != nil {
		t.Skip("no cpio(1)")
	}
	dir := t.TempDir()
	build(t, dir)
	out, err := exec.Command("cpio", "-itv", "--quiet", "-F", filepath.Join(dir, "init.cpio")).CombinedOutput()
	if err != nil {
		t.Fatalf("cpio rejected the bundle: %v\n%s", err, out)
	}
	for _, want := range []string{"init", "bin/zpinit", "dev/console"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("cpio listing lacks %s:\n%s", want, out)
		}
	}
}

func TestRejectsBadPaths(t *testing.T) {
	src := filepath.Join(t.TempDir(), "f")
	os.WriteFile(src, nil, 0o600)
	for _, name := range []string{"/init", "../x", "etc/passwd"} {
		if err := Write(filepath.Join(t.TempDir(), "o"), map[string]string{name: src}); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}
