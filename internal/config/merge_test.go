package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func mustMerge(t *testing.T, base, override string) *Config {
	t.Helper()
	merged, err := Merge([]byte(base), []byte(override))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	cfg, err := Parse(merged)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

const minimalBase = `{
  "hostname": "web-01",
  "image": {
    "entrypoint": ["/docker-entrypoint.sh"],
    "cmd": ["nginx", "-g", "daemon off;"],
    "env": ["PATH=/usr/bin", "NGINX_VERSION=1.31.3"],
    "user": "nginx",
    "working_dir": "/"
  },
  "network": {
    "interfaces": [{"name": "eth0", "address": "172.16.0.2/30", "gateway": "172.16.0.1", "mmds": true}],
    "nameservers": ["1.1.1.1"]
  },
  "boot": {"profile": "zpinit", "args": ["--vm"]}
}`

func TestMergeReplacesTopLevelKeyWholesale(t *testing.T) {
	// A network stated in an override is a complete restatement, not a
	// patch: fctl owns the whole block per launch.
	cfg := mustMerge(t, minimalBase, `{"network": {"interfaces": [{"name": "eth0", "address": "10.0.0.5/24"}]}}`)

	if got := len(cfg.Network.Interfaces); got != 1 {
		t.Fatalf("interfaces = %d, want 1", got)
	}
	if got := cfg.Network.Interfaces[0].Address; got != "10.0.0.5/24" {
		t.Errorf("address = %q, want 10.0.0.5/24", got)
	}
	if got := cfg.Network.Interfaces[0].Gateway; got != "" {
		t.Errorf("gateway = %q, want empty: the override restated the block without one", got)
	}
	if got := len(cfg.Network.Nameservers); got != 0 {
		t.Errorf("nameservers = %v, want none: they were part of the replaced block", cfg.Network.Nameservers)
	}
}

func TestMergeImageIsPerField(t *testing.T) {
	// An override that changes the user must not blank out the entrypoint
	// it said nothing about.
	cfg := mustMerge(t, minimalBase, `{"image": {"user": "root"}}`)

	if got := cfg.Image.User; got != "root" {
		t.Errorf("user = %q, want root", got)
	}
	if want := []string{"/docker-entrypoint.sh"}; !reflect.DeepEqual(cfg.Image.Entrypoint, want) {
		t.Errorf("entrypoint = %v, want %v", cfg.Image.Entrypoint, want)
	}
	if want := []string{"nginx", "-g", "daemon off;"}; !reflect.DeepEqual(cfg.Image.Cmd, want) {
		t.Errorf("cmd = %v, want %v", cfg.Image.Cmd, want)
	}
}

func TestMergeImageEnvIsPerVariableAndKeepsOrder(t *testing.T) {
	cfg := mustMerge(t, minimalBase, `{"image": {"env": ["PATH=/usr/local/bin", "TZ=UTC"]}}`)

	want := []string{"PATH=/usr/local/bin", "NGINX_VERSION=1.31.3", "TZ=UTC"}
	if !reflect.DeepEqual(cfg.Image.Env, want) {
		t.Errorf("env = %v, want %v (PATH replaced in place, TZ appended)", cfg.Image.Env, want)
	}
}

func TestMergeExtraEnvIsPerVariable(t *testing.T) {
	base := `{"image": {}, "extra_env": {"A": "1", "B": "2"}, "boot": {"profile": "zpinit"}}`
	cfg := mustMerge(t, base, `{"extra_env": {"B": "two", "C": "3"}}`)

	want := map[string]string{"A": "1", "B": "two", "C": "3"}
	if !reflect.DeepEqual(cfg.ExtraEnv, want) {
		t.Errorf("extra_env = %v, want %v", cfg.ExtraEnv, want)
	}
}

func TestMergeDistinguishesAbsentFromEmpty(t *testing.T) {
	base := `{"image": {}, "extra_env": {"A": "1"}, "boot": {"profile": "zpinit"}}`

	if cfg := mustMerge(t, base, `{}`); len(cfg.ExtraEnv) != 1 {
		t.Errorf("an override saying nothing about extra_env changed it to %v", cfg.ExtraEnv)
	}
	// An explicit empty object is a statement, and per-variable merging
	// makes it a no-op rather than a reset. This is the one place where
	// "shallow per top-level key" and "env merges per variable" disagree;
	// the env rule wins, as specified.
	if cfg := mustMerge(t, base, `{"extra_env": {}}`); len(cfg.ExtraEnv) != 1 {
		t.Errorf("extra_env = %v, want the base preserved", cfg.ExtraEnv)
	}
}

func TestMergeRejectsMalformedOverride(t *testing.T) {
	// A malformed override must never degrade to the base config: it is
	// where the network arrives from fctl, and a VM booting with the wrong
	// address is worse than one that refuses to boot.
	if _, err := Merge([]byte(minimalBase), []byte(`{"network":`)); err == nil {
		t.Fatal("Merge accepted a truncated override")
	}
	if _, err := Merge([]byte(minimalBase), []byte(`["not", "an", "object"]`)); err == nil {
		t.Fatal("Merge accepted a JSON array as an override")
	}
}

func TestMergeEmptyOverrideIsIdentity(t *testing.T) {
	merged, err := Merge([]byte(minimalBase), nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(minimalBase), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}

func TestArgvAndUserPrecedence(t *testing.T) {
	cfg := mustMerge(t, minimalBase, `{"exec_override": ["/bin/sh", "-c", "sleep 1"], "user_override": "1000:1000"}`)

	// exec_override replaces entrypoint and cmd wholesale, matching
	// Docker's --entrypoint semantics.
	if want := []string{"/bin/sh", "-c", "sleep 1"}; !reflect.DeepEqual(cfg.Argv(), want) {
		t.Errorf("Argv = %v, want %v", cfg.Argv(), want)
	}
	if got := cfg.User(); got != "1000:1000" {
		t.Errorf("User = %q, want 1000:1000", got)
	}

	plain := mustMerge(t, minimalBase, `{}`)
	if want := []string{"/docker-entrypoint.sh", "nginx", "-g", "daemon off;"}; !reflect.DeepEqual(plain.Argv(), want) {
		t.Errorf("Argv = %v, want entrypoint followed by cmd", plain.Argv())
	}
	if got := plain.User(); got != "nginx" {
		t.Errorf("User = %q, want nginx", got)
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`{"image": {}, "boot": {"profile": "zpinit"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Boot.Path != DefaultZpinitPath {
		t.Errorf("boot.path = %q, want %q", cfg.Boot.Path, DefaultZpinitPath)
	}
	// boot.args stays empty on purpose: injecting --vm would break older
	// zpinit releases that reject unknown flags.
	if len(cfg.Boot.Args) != 0 {
		t.Errorf("boot.args = %v, want empty", cfg.Boot.Args)
	}
	if cfg.Logging.Target != LogSerial {
		t.Errorf("logging.target = %q, want serial", cfg.Logging.Target)
	}
	if cfg.MMDS.Address != DefaultMMDSAddress || cfg.MMDS.Version != DefaultMMDSVersion {
		t.Errorf("mmds = %+v, want the documented defaults", cfg.MMDS)
	}
}

func TestParseRejects(t *testing.T) {
	for name, doc := range map[string]string{
		"no profile":        `{"image": {}}`,
		"unknown profile":   `{"image": {}, "boot": {"profile": "runit"}}`,
		"mmds v1":           `{"image": {}, "boot": {"profile": "zpinit"}, "mmds": {"version": "V1"}}`,
		"unknown log":       `{"image": {}, "boot": {"profile": "zpinit"}, "logging": {"target": "vsock"}}`,
		"unknown field":     `{"image": {}, "boot": {"profile": "zpinit"}, "hostnam": "typo"}`,
		"nameless iface":    `{"image": {}, "boot": {"profile": "zpinit"}, "network": {"interfaces": [{"address": "10.0.0.1/24"}]}}`,
		"duplicate iface":   `{"image": {}, "boot": {"profile": "zpinit"}, "network": {"interfaces": [{"name": "eth0"}, {"name": "eth0"}]}}`,
		"incomplete mount":  `{"image": {}, "boot": {"profile": "zpinit"}, "mounts": [{"device": "/dev/vdc"}]}`,
		"overlay no fstype": `{"image": {}, "boot": {"profile": "zpinit"}, "overlay_root": {"lower": {"device": "/dev/vda"}, "upper": {"device": "tmpfs"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Errorf("Parse accepted %s", doc)
			}
		})
	}
}

func TestLoadMissingBaseIsFatalAndMissingOverrideIsNot(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.json")
	override := filepath.Join(dir, "override.json")

	if _, err := Load(base, override); err == nil {
		t.Fatal("Load succeeded with no base config")
	}

	if err := os.WriteFile(base, []byte(minimalBase), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(base, override)
	if err != nil {
		t.Fatalf("Load with no override: %v", err)
	}
	if cfg.Hostname != "web-01" {
		t.Errorf("hostname = %q, want web-01", cfg.Hostname)
	}

	if err := os.WriteFile(override, []byte(`{"hostname": "web-02"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(base, override); err != nil {
		t.Fatalf("Load with override: %v", err)
	}
	if cfg.Hostname != "web-02" {
		t.Errorf("hostname = %q, want web-02", cfg.Hostname)
	}
}

func TestProfileApply(t *testing.T) {
	for p, want := range map[Profile]bool{
		ProfileZpinit:    true,
		ProfileCommand:   true,
		ProfileCloudInit: false,
		ProfileSystemd:   false,
		ProfileOpenRC:    false,
	} {
		if got := p.Apply(); got != want {
			t.Errorf("%s.Apply() = %t, want %t", p, got, want)
		}
	}
}
