package identity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0ploy/vminit/internal/config"
)

func TestHostsFile(t *testing.T) {
	cfg := &config.Config{
		Hostname: "web-01",
		Network: config.Network{
			Interfaces: []config.Interface{{Name: "eth0", Address: "172.16.0.2/30"}},
			ExtraHosts: []config.HostEntry{{IP: "10.0.0.5", Host: "db.internal"}},
		},
	}
	want := "127.0.0.1\tlocalhost\n" +
		"::1\tlocalhost ip6-localhost ip6-loopback\n" +
		"172.16.0.2\tweb-01\n" +
		"10.0.0.5\tdb.internal\n"
	if got := hostsFile(cfg); got != want {
		t.Errorf("hostsFile:\n%q\nwant:\n%q", got, want)
	}
}

func TestHostsFileWithoutAddress(t *testing.T) {
	// No configured address means no self-mapping line, not a mapping to a
	// bare or malformed address.
	cfg := &config.Config{Hostname: "web-01"}
	want := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if got := hostsFile(cfg); got != want {
		t.Errorf("hostsFile:\n%q\nwant:\n%q", got, want)
	}
}

func TestResolvConf(t *testing.T) {
	nc := config.Network{Nameservers: []string{"1.1.1.1", "8.8.8.8"}, Search: []string{"internal", "example.com"}}
	want := "search internal example.com\nnameserver 1.1.1.1\nnameserver 8.8.8.8\n"
	if got := resolvConf(nc); got != want {
		t.Errorf("resolvConf:\n%q\nwant:\n%q", got, want)
	}

	// No nameservers produces nothing at all, so the caller can warn
	// instead of writing an empty resolv.conf that looks configured.
	if got := resolvConf(config.Network{Search: []string{"internal"}}); got != "" {
		t.Errorf("resolvConf with no nameservers = %q, want empty", got)
	}
}

func TestWriteReplacesSymlink(t *testing.T) {
	// Some images ship /etc/resolv.conf as a symlink. Writing through it
	// would land the content somewhere else entirely, so the rename has to
	// replace the link itself.
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "resolv.conf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := write(link, "nameserver 1.1.1.1\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("path is still a symlink")
	}
	if got, _ := os.ReadFile(target); string(got) != "untouched" {
		t.Errorf("the symlink target was overwritten: %q", got)
	}
	if got, _ := os.ReadFile(link); string(got) != "nameserver 1.1.1.1\n" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteIsIdempotent(t *testing.T) {
	// Docker leaves these files as 0-byte regular files, and the agent
	// rewrites them when MMDS changes, so truncate-and-replace rather than
	// append is what makes re-running safe.
	path := filepath.Join(t.TempDir(), "hosts")
	for range 3 {
		if err := write(path, "127.0.0.1\tlocalhost\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "127.0.0.1\tlocalhost\n" {
		t.Errorf("content = %q, want one copy", got)
	}
}
