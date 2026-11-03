package config

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
	"time"
)

const validDoc = `{
  "hostname": "web-01",
  "handoff": {
    "command": ["/docker-entrypoint.sh", "nginx", "-g", "daemon off;"],
    "user": "nginx",
    "working_dir": "/",
    "stop_signal": "SIGQUIT",
    "stop_timeout": "10s"
  },
  "env": ["PATH=/usr/bin:/bin", "A=1"],
  "network": {
    "interfaces": [{"name": "eth0", "mac": "06:00:AC:10:00:02", "address": "10.77.0.5/32", "gateway": "169.254.0.1", "mmds": false, "mtu": 1480}],
    "nameservers": ["1.1.1.1"],
    "search": [],
    "extra_hosts": [{"ip": "10.0.0.5", "host": "db.internal"}]
  },
  "root": {"device": "/dev/vdb", "fstype": "ext4", "options": "discard"},
  "mounts": [{"device": "/dev/vdc", "path": "/data", "fstype": "ext4", "options": "relatime"}],
  "exec": {"port": 1024, "user": "nginx", "working_dir": "/"},
  "logging": {"target": "serial"},
  "boot_timer": true
}`

// superviseHandoff is the handoff of a zpinit-native image: no command, and
// so no user or working directory either.
const superviseHandoff = `"handoff": {
    "stop_signal": "SIGTERM",
    "stop_timeout": "30s"
  }`

const wrapHandoff = `"handoff": {
    "command": ["/docker-entrypoint.sh", "nginx", "-g", "daemon off;"],
    "user": "nginx",
    "working_dir": "/",
    "stop_signal": "SIGQUIT",
    "stop_timeout": "10s"
  }`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(c.Handoff.StopTimeout); got != 10*time.Second {
		t.Errorf("stop_timeout = %v", got)
	}
	if !c.Handoff.Wrap() || c.Handoff.Command[0] != "/docker-entrypoint.sh" {
		t.Errorf("handoff = %+v", c.Handoff)
	}
	if c.Root.Device != "/dev/vdb" || c.Root.Options != "discard" || c.Exec.Port != 1024 ||
		c.Network.Interfaces[0].MTU != 1480 || !c.BootTimer {
		t.Errorf("decoded %+v", c)
	}
}

func TestParseSupervise(t *testing.T) {
	c, err := Parse(edit(t, wrapHandoff, superviseHandoff))
	if err != nil {
		t.Fatal(err)
	}
	if c.Handoff.Wrap() {
		t.Error("a handoff without command must be supervise mode")
	}
}

// edit applies one textual change to validDoc, failing the test if the
// pattern is absent so a reworded fixture cannot make a case vacuous.
func edit(t *testing.T, old, new string) []byte {
	t.Helper()
	if !strings.Contains(validDoc, old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	return []byte(strings.Replace(validDoc, old, new, 1))
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name, old, new, want string
	}{
		{"unknown top-level field", `"hostname"`, `"image": {}, "hostname"`, `unknown field "image"`},
		{"old overlay_root", `"hostname"`, `"overlay_root": {}, "hostname"`, `unknown field "overlay_root"`},
		{"old handoff.argv", `"user": "nginx",`, `"user": "nginx", "argv": ["/x"],`, `unknown field "argv"`},
		{"old handoff.env_file", `"user": "nginx",`, `"user": "nginx", "env_file": "/run/zpinit/env",`, `unknown field "env_file"`},
		{"trailing data", `true
}`, `true
} {}`, "trailing data"},
		{"empty command", `["/docker-entrypoint.sh", "nginx", "-g", "daemon off;"]`, `[]`, "handoff.command must be non-empty"},
		{"empty command[0]", `["/docker-entrypoint.sh",`, `["",`, "handoff.command[0]"},
		{"command without user", `"user": "nginx",
    "working_dir"`, `"working_dir"`, "handoff.user is required"},
		{"relative working_dir", `"working_dir": "/",
    "stop_signal"`, `"working_dir": "app",
    "stop_signal"`, "handoff.working_dir"},
		{"supervise with user", wrapHandoff, `"handoff": {"user": "root", "stop_signal": "SIGTERM", "stop_timeout": "30s"}`, "only valid with handoff.command"},
		{"supervise with working_dir", wrapHandoff, `"handoff": {"working_dir": "/", "stop_signal": "SIGTERM", "stop_timeout": "30s"}`, "only valid with handoff.command"},
		{"bad signal", `"SIGQUIT"`, `"SIGNOPE"`, "handoff.stop_signal"},
		{"missing signal", `"stop_signal": "SIGQUIT",`, ``, "handoff.stop_signal is required"},
		{"bad duration", `"10s"`, `"ten"`, "duration"},
		{"zero timeout", `"10s"`, `"0s"`, "handoff.stop_timeout"},
		{"env without =", `"A=1"`, `"A"`, "env[1]"},
		{"env with empty key", `"A=1"`, `"=1"`, "env[1]"},
		{"missing root", `"root": {"device": "/dev/vdb", "fstype": "ext4", "options": "discard"},`, ``, "root: device and fstype"},
		{"relative root device", `"device": "/dev/vdb"`, `"device": "vdb"`, "root.device"},
		{"read-only root", `"options": "discard"`, `"options": "discard,ro"`, "must not contain ro"},
		{"relative mount path", `"path": "/data"`, `"path": "data"`, "mounts[0].path"},
		{"exec without port", `"port": 1024`, `"port": 0`, "exec.port"},
		{"bad log target", `"serial"`, `"syslog"`, "logging.target"},
		{"missing log target", `"target": "serial"`, `"target": ""`, "logging.target is required"},
		{"mtu too small", `"mtu": 1480`, `"mtu": 67`, "mtu 67 is outside"},
		{"mtu too large", `"mtu": 1480`, `"mtu": 65536`, "mtu 65536 is outside"},
		{"negative mtu", `"mtu": 1480`, `"mtu": -1`, "mtu -1 is outside"},
		{"boot_timer not a bool", `"boot_timer": true`, `"boot_timer": "yes"`, "boot_timer"},
		{"duplicate interface", `"interfaces": [{"name": "eth0",`, `"interfaces": [{"name": "eth0"}, {"name": "eth0",`, "duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(edit(t, tt.old, tt.new))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestBlobRoundTrip(t *testing.T) {
	b, err := Encode([]byte(validDoc))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != DeviceSize {
		t.Errorf("blob is %d bytes, want %d", len(b), DeviceSize)
	}
	doc, err := Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if string(doc) != validDoc {
		t.Error("document changed in the round trip")
	}
}

func TestDecodeRejects(t *testing.T) {
	good, _ := Encode([]byte(`{}`))
	mutate := func(f func(b []byte) []byte) []byte {
		return f(append([]byte(nil), good...))
	}
	tests := []struct {
		name string
		blob []byte
		want string
	}{
		{"short header", good[:10], "read header"},
		{"wrong magic", mutate(func(b []byte) []byte { copy(b, "EXT4\x00\x00\x00\x00"); return b }), "not the config device"},
		{"zeroed device", make([]byte, DeviceSize), "not the config device"},
		{"version 2", mutate(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[8:], 2); return b }), "version 2"},
		{"zero length", mutate(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[12:], 0); return b }), "empty"},
		{"oversized length", mutate(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[12:], DeviceSize); return b }), "exceeds"},
		{"runs past the end", mutate(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[12:], 100); return b[:HeaderSize+50] }), "runs past"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(bytes.NewReader(tt.blob))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestEncodeRejectsOversize(t *testing.T) {
	if _, err := Encode(make([]byte, MaxDocument+1)); err == nil {
		t.Error("accepted a document larger than the device")
	}
}

func TestDeviceFromCmdline(t *testing.T) {
	tests := []struct {
		cmdline, want, err string
	}{
		{"reboot=k panic=1 vmsetup_config=/dev/vda quiet", "/dev/vda", ""},
		{"vmsetup_config=/dev/vdb", "/dev/vdb", ""},
		{"reboot=k init=/init", "", "no vmsetup_config="},
		{"vmsetup_config=/dev/vda vmsetup_config=/dev/vdb", "", "2 times"},
		{"vmsetup_config=vda", "", "not a /dev path"},
		{"init=/init -- vmsetup_config=/dev/vda", "", "no vmsetup_config="},
		// The dotted module-parameter form is a different parameter.
		{"vmsetup.config=/dev/vda", "", "no vmsetup_config="},
	}
	for _, tt := range tests {
		got, err := DeviceFromCmdline(tt.cmdline)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%q: err = %v, want %q", tt.cmdline, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%q: got %q, %v; want %q", tt.cmdline, got, err, tt.want)
		}
	}
}

// TestBlobWriterReader is the contract between fctl and vmsetup: what the
// writer's Blob produces, the guest's Decode and Parse accept unchanged.
func TestBlobWriterReader(t *testing.T) {
	want, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's "search": [] is written back as absent; both mean none.
	want.Network.Search = nil
	b, err := want.Blob()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != DeviceSize {
		t.Fatalf("blob is %d bytes, want %d", len(b), DeviceSize)
	}
	doc, err := Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the config:\n got %+v\nwant %+v", got, want)
	}
}

func TestBlobValidates(t *testing.T) {
	c, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatal(err)
	}
	c.Root.Options = "ro"
	if _, err := c.Blob(); err == nil || !strings.Contains(err.Error(), "must not contain ro") {
		t.Fatalf("Blob wrote an invalid config: %v", err)
	}
}
