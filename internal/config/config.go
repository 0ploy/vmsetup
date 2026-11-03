// Package config defines the guest config: the one JSON document fctl
// composes for each launch and vmsetup reads at boot. See SPEC.md §4.
//
// vmsetup owns the schema and fctl writes it. Drift between the writer and the
// reader is the likeliest bug in the design, so the document carries only
// resolved values (principle 7: no merging in the guest) and decoding is
// strict -- a field vmsetup does not know is an error, never silently ignored.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Config is the whole guest config, SPEC.md §4.2.
type Config struct {
	Hostname string `json:"hostname,omitempty"`

	// Handoff is what zpinit runs once vmsetup execs it.
	Handoff Handoff `json:"handoff"`

	// Env is the composed environment, final, in OCI KEY=value form.
	// Order is significant: a later entry for the same key wins.
	Env []string `json:"env,omitempty"`

	Network Network `json:"network"`

	// Root is the VM's disk. vmsetup mounts it and switches root into it
	// (SPEC.md §5).
	Root Root `json:"root"`

	// Mounts are extra data volumes, mounted after the standard table.
	Mounts []Mount `json:"mounts,omitempty"`

	// Exec, when set, is passed to zpinit, which listens for exec requests
	// over vsock. Absent means no listener. zpinit#10.
	Exec *Exec `json:"exec,omitempty"`

	Logging Logging `json:"logging"`
}

// Handoff is fully resolved by fctl: Docker's --entrypoint / --user / -w /
// command semantics have already been applied host-side (SPEC.md §10.1).
//
// Command decides zpinit's mode. Present: wrap mode, zpinit runs it as its
// one child, as User in WorkingDir. Absent: supervise mode, the image's own
// /etc/zpinit services, and then User and WorkingDir must be absent too,
// because they live in that service TOML.
type Handoff struct {
	// Command is the Docker command, entrypoint + cmd, argv form.
	Command []string `json:"command,omitempty"`
	// User is user[:group], names or numeric ids. With Command only.
	User string `json:"user,omitempty"`
	// WorkingDir is absolute. With Command only.
	WorkingDir string `json:"working_dir,omitempty"`
	// StopSignal is what zpinit sends the command on graceful stop.
	StopSignal string `json:"stop_signal"`
	// StopTimeout is zpinit's whole graceful-stop budget. fctl keeps it
	// below its own stop.timeout_sec.
	StopTimeout Duration `json:"stop_timeout"`
}

// Wrap reports whether zpinit runs a single command (wrap mode) rather than
// the image's services (supervise mode).
func (h Handoff) Wrap() bool { return len(h.Command) > 0 }

type Network struct {
	Interfaces  []Interface `json:"interfaces,omitempty"`
	Nameservers []string    `json:"nameservers,omitempty"`
	Search      []string    `json:"search,omitempty"`
	ExtraHosts  []HostEntry `json:"extra_hosts,omitempty"`
}

type Interface struct {
	Name string `json:"name"`
	// MAC pins the link by hardware address. Firecracker does not
	// guarantee NIC enumeration order matches API call order.
	MAC string `json:"mac,omitempty"`
	// Address is the interface address in CIDR form, e.g. 10.77.0.5/32.
	Address string `json:"address,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	// MMDS adds a host route to the MMDS address over this link.
	MMDS bool `json:"mmds,omitempty"`
}

type HostEntry struct {
	IP   string `json:"ip"`
	Host string `json:"host"`
}

type Mount struct {
	Device  string `json:"device"`
	Path    string `json:"path"`
	FSType  string `json:"fstype"`
	Options string `json:"options,omitempty"`
}

// Root is the VM's disk: a writable, per-VM copy of the image, which fctl
// attached and names here. vmsetup never guesses.
type Root struct {
	Device  string `json:"device"`
	FSType  string `json:"fstype"`
	Options string `json:"options,omitempty"`
}

type Exec struct {
	// Port is the vsock port zpinit listens on.
	Port uint32 `json:"port"`
	// User is the default identity for an exec request: the image's OCI
	// User, not the handoff user.
	User       string `json:"user"`
	WorkingDir string `json:"working_dir"`
}

type LogTarget string

const (
	// LogSerial writes to /dev/ttyS0, which Firecracker forwards to a
	// per-VM file on the host. Works without console= (SPEC.md §9).
	LogSerial LogTarget = "serial"
	LogNone   LogTarget = "none"
)

type Logging struct {
	Target LogTarget `json:"target"`
}

// MMDSAddress is where Firecracker's MMDS answers. It is fixed by
// Firecracker's default, so it is not a config field: one more value fctl
// and vmsetup could disagree on buys nothing.
const MMDSAddress = "169.254.169.254"

// Duration is a time.Duration that encodes as a Go duration string ("10s").
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Parse decodes and validates a config document. Unknown fields and trailing
// data are errors.
func Parse(doc []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode config: trailing data after the document")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate rejects a document that cannot produce a correct machine, naming
// the field. Every violation is a host-side composition bug; vmsetup parks on
// it (SPEC.md §4.3, principle 5).
//
// Nothing is defaulted. Every field anything acts on arrives resolved, and a
// required field that is missing is an error rather than a guess.
func (c *Config) Validate() error {
	h := c.Handoff
	if h.Wrap() {
		if h.Command[0] == "" {
			return errors.New("handoff.command[0] must be non-empty")
		}
		if h.User == "" {
			return errors.New("handoff.user is required with handoff.command")
		}
		if !path.IsAbs(h.WorkingDir) {
			return fmt.Errorf("handoff.working_dir %q must be an absolute path", h.WorkingDir)
		}
	} else if h.Command != nil {
		return errors.New("handoff.command must be non-empty when present; omit it for supervise mode")
	} else if h.User != "" || h.WorkingDir != "" {
		return errors.New("handoff.user and handoff.working_dir are only valid with handoff.command; in supervise mode they live in the image's zpinit service TOML")
	}
	if h.StopSignal == "" {
		return errors.New("handoff.stop_signal is required")
	}
	if unix.SignalNum(h.StopSignal) == 0 {
		return fmt.Errorf("handoff.stop_signal %q is not a signal name vmsetup knows (want e.g. SIGTERM)", h.StopSignal)
	}
	if h.StopTimeout <= 0 {
		return errors.New("handoff.stop_timeout is required and must be positive")
	}

	r := c.Root
	if r.Device == "" || r.FSType == "" {
		return errors.New("root: device and fstype are required")
	}
	if !path.IsAbs(r.Device) {
		return fmt.Errorf("root.device %q must be an absolute path", r.Device)
	}
	for _, o := range strings.Split(r.Options, ",") {
		if strings.TrimSpace(o) == "ro" {
			return errors.New("root.options must not contain ro: the root is the VM's writable disk")
		}
	}

	for i, e := range c.Env {
		k, _, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			return fmt.Errorf("env[%d] %q is not in KEY=value form", i, e)
		}
	}

	seen := make(map[string]struct{}, len(c.Network.Interfaces))
	for i, iface := range c.Network.Interfaces {
		if iface.Name == "" {
			return fmt.Errorf("network.interfaces[%d].name is required", i)
		}
		if _, dup := seen[iface.Name]; dup {
			return fmt.Errorf("network.interfaces[%d]: duplicate name %q", i, iface.Name)
		}
		seen[iface.Name] = struct{}{}
	}

	for i, m := range c.Mounts {
		if m.Device == "" || m.FSType == "" {
			return fmt.Errorf("mounts[%d]: device and fstype are required", i)
		}
		if !path.IsAbs(m.Path) {
			return fmt.Errorf("mounts[%d].path %q must be an absolute path", i, m.Path)
		}
	}

	if e := c.Exec; e != nil {
		if e.Port == 0 {
			return errors.New("exec.port is required")
		}
		if e.User == "" {
			return errors.New("exec.user is required")
		}
		if !path.IsAbs(e.WorkingDir) {
			return fmt.Errorf("exec.working_dir %q must be an absolute path", e.WorkingDir)
		}
	}

	switch c.Logging.Target {
	case LogSerial, LogNone:
	case "":
		return errors.New("logging.target is required (serial or none)")
	default:
		return fmt.Errorf("logging.target %q unknown (want serial or none)", c.Logging.Target)
	}
	return nil
}
