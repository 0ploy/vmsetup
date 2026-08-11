// Package config defines the configuration schema shared by vminit (which
// reads it inside the guest) and vmpack (which writes it at image build
// time). Co-locating the reader and the writer is the whole reason those two
// binaries live in one repository: drift between them is the likeliest bug in
// the design.
//
// The schema is specified in PLAN.md §4.2. Three layers merge into it:
// build-time defaults at /etc/vminit/config.json, a boot-time override at
// /etc/vminit/override.json written by fctl, and live MMDS updates applied by
// `vminit agent`. See merge.go for the merge rules.
package config

import "fmt"

// Profile selects the handoff target, and with it who configures the
// machine's network. See PLAN.md §4.3.
type Profile string

const (
	// ProfileZpinit brings the machine up and execve's zpinit, which
	// becomes PID 1.
	ProfileZpinit Profile = "zpinit"
	// ProfileCommand brings the machine up and forks the image's
	// entrypoint, staying PID 1 to reap and own the machine lifecycle.
	ProfileCommand Profile = "command"
	// ProfileCloudInit pins the cloud-init datasource and execs the
	// image's init; cloud-init configures the network.
	ProfileCloudInit Profile = "cloud-init"
	// ProfileSystemd writes network config for systemd's network stack,
	// enables it, and execs the image's init.
	ProfileSystemd Profile = "systemd"
	// ProfileOpenRC writes /etc/network/interfaces, enables OpenRC's
	// networking service, and execs the image's init.
	ProfileOpenRC Profile = "openrc"
)

// Apply reports whether the profile makes vminit configure the machine
// imperatively (mounts, netlink, identity files). The generate profiles
// return false: systemd and OpenRC own mounts, cgroups and network
// management, so vminit must write files they will consume and touch
// nothing else.
func (p Profile) Apply() bool {
	return p == ProfileZpinit || p == ProfileCommand
}

// Config is the merged result of every configuration layer.
type Config struct {
	Hostname string `json:"hostname,omitempty"`

	// Image holds the OCI image config verbatim, as vmpack found it.
	Image Image `json:"image"`

	// ExecOverride replaces Image.Entrypoint and Image.Cmd wholesale,
	// matching `docker run --entrypoint`. An empty slice means unset.
	ExecOverride []string `json:"exec_override,omitempty"`
	// UserOverride replaces Image.User, matching `docker run --user`.
	UserOverride string `json:"user_override,omitempty"`
	// ExtraEnv layers over Image.Env, matching `docker run -e`.
	ExtraEnv map[string]string `json:"extra_env,omitempty"`

	Network Network `json:"network"`

	// Mounts are extra data volumes, applied after the standard mount
	// table.
	Mounts []Mount `json:"mounts,omitempty"`

	// OverlayRoot, when set, makes vminit assemble an overlay root and
	// pivot_root into it before mounting anything else. See PLAN.md §5.2.
	OverlayRoot *OverlayRoot `json:"overlay_root,omitempty"`

	Boot    Boot    `json:"boot"`
	Logging Logging `json:"logging"`
	MMDS    MMDS    `json:"mmds"`
}

// Image is the subset of the OCI image config that matters to boot.
type Image struct {
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	// Env is in OCI form: a list of KEY=value strings, order significant.
	Env        []string `json:"env,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	User       string   `json:"user,omitempty"`
	StopSignal string   `json:"stop_signal,omitempty"`
}

// Argv returns the command to run: ExecOverride if set, else Entrypoint
// followed by Cmd. This mirrors Docker's semantics, where --entrypoint
// replaces both halves rather than only the first.
func (c *Config) Argv() []string {
	if len(c.ExecOverride) > 0 {
		return c.ExecOverride
	}
	argv := make([]string, 0, len(c.Image.Entrypoint)+len(c.Image.Cmd))
	argv = append(argv, c.Image.Entrypoint...)
	return append(argv, c.Image.Cmd...)
}

// User returns the user spec to resolve: UserOverride if set, else the
// image's User, else root.
func (c *Config) User() string {
	if c.UserOverride != "" {
		return c.UserOverride
	}
	if c.Image.User != "" {
		return c.Image.User
	}
	return "root"
}

type Network struct {
	Interfaces  []Interface `json:"interfaces,omitempty"`
	Nameservers []string    `json:"nameservers,omitempty"`
	Search      []string    `json:"search,omitempty"`
	ExtraHosts  []HostEntry `json:"extra_hosts,omitempty"`
}

type Interface struct {
	Name string `json:"name"`
	// MAC pins the link by hardware address. Firecracker does not
	// guarantee that NIC enumeration order matches the order the API was
	// called in, so set this whenever there is more than one interface.
	MAC string `json:"mac,omitempty"`
	// Address is the interface address in CIDR form, e.g. 172.16.0.2/30.
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

type OverlayRoot struct {
	Lower   Layer  `json:"lower"`
	Upper   Layer  `json:"upper"`
	WorkDir string `json:"workdir,omitempty"`
}

// Layer is one half of an overlay root. Device is a block device path, or
// the literal "tmpfs" for an ephemeral upper layer.
type Layer struct {
	Device string `json:"device"`
	FSType string `json:"fstype"`
}

type Boot struct {
	Profile Profile `json:"profile"`

	// Path and Args are the handoff target for profile "zpinit".
	Path string   `json:"path,omitempty"`
	Args []string `json:"args,omitempty"`

	// Network selects the config format for profile "systemd" only:
	// auto | netplan | systemd-networkd | ifupdown.
	Network string `json:"network,omitempty"`

	// Init is the image's init, exec'd by the generate profiles.
	Init string `json:"init,omitempty"`
}

type LogTarget string

const (
	// LogSerial writes to /dev/ttyS0, which Firecracker forwards to a
	// per-VM file on the host. See PLAN.md §7.2 for why this does not
	// need console= on the kernel command line.
	LogSerial LogTarget = "serial"
	LogNone   LogTarget = "none"
)

type Logging struct {
	Target LogTarget `json:"target,omitempty"`
}

type MMDS struct {
	// Enabled turns on the `vminit agent` poll loop. It does not affect
	// boot: the MMDS route is requested per interface instead.
	Enabled bool   `json:"enabled,omitempty"`
	Address string `json:"address,omitempty"`
	// Version must be V2. V1 is unauthenticated, so any process in the
	// guest can read the metadata; PLAN.md §9 forbids falling back to it
	// even when the host offers it.
	Version string `json:"version,omitempty"`
	// PollInterval is a Go duration string, e.g. "30s".
	PollInterval string `json:"poll_interval,omitempty"`
}

// Defaults for the values that have a single sensible answer. Anything
// where guessing wrong would boot a subtly wrong machine is left unset and
// rejected by Validate instead.
const (
	DefaultZpinitPath  = "/usr/local/bin/zpinit"
	DefaultInit        = "/sbin/init"
	DefaultMMDSAddress = "169.254.169.254"
	DefaultMMDSVersion = "V2"
	DefaultPollInteval = "30s"
	DefaultOverlayWork = "/overlay/work"
)

// ApplyDefaults fills in the values that are safe to assume.
//
// boot.args is deliberately not defaulted. The zpinit profile needs
// --vm to make zpinit reboot rather than exit as PID 1, but inventing a
// flag for a binary we only exec would silently break older zpinit
// releases that do not accept it. vmpack writes the flag; vminit warns at
// boot when it is missing (see checkZpinitArgs in cmd/vminit).
func (c *Config) ApplyDefaults() {
	if c.Boot.Profile == ProfileZpinit && c.Boot.Path == "" {
		c.Boot.Path = DefaultZpinitPath
	}
	if !c.Boot.Profile.Apply() && c.Boot.Init == "" {
		c.Boot.Init = DefaultInit
	}
	if c.Boot.Profile == ProfileSystemd && c.Boot.Network == "" {
		c.Boot.Network = "auto"
	}
	if c.Logging.Target == "" {
		c.Logging.Target = LogSerial
	}
	if c.MMDS.Address == "" {
		c.MMDS.Address = DefaultMMDSAddress
	}
	if c.MMDS.Version == "" {
		c.MMDS.Version = DefaultMMDSVersion
	}
	if c.MMDS.PollInterval == "" {
		c.MMDS.PollInterval = DefaultPollInteval
	}
	if c.OverlayRoot != nil && c.OverlayRoot.WorkDir == "" {
		c.OverlayRoot.WorkDir = DefaultOverlayWork
	}
}

// Validate rejects a configuration that cannot produce a correct machine.
// It is deliberately strict: principle 5 in PLAN.md §2 is that a
// misconfigured network which boots anyway is worse than a VM that refuses
// to start with a clear message.
func (c *Config) Validate() error {
	switch c.Boot.Profile {
	case ProfileZpinit, ProfileCommand, ProfileCloudInit, ProfileSystemd, ProfileOpenRC:
	case "":
		return fmt.Errorf("boot.profile is required")
	default:
		return fmt.Errorf("boot.profile %q unknown (want zpinit, command, cloud-init, systemd or openrc)", c.Boot.Profile)
	}

	switch c.Logging.Target {
	case LogSerial, LogNone:
	default:
		return fmt.Errorf("logging.target %q unknown (want serial or none)", c.Logging.Target)
	}

	if c.MMDS.Version != DefaultMMDSVersion {
		return fmt.Errorf("mmds.version %q refused: V1 is unauthenticated, so any process in the guest can read the metadata", c.MMDS.Version)
	}

	seen := make(map[string]struct{}, len(c.Network.Interfaces))
	for i, iface := range c.Network.Interfaces {
		if iface.Name == "" {
			return fmt.Errorf("network.interfaces[%d]: name is required", i)
		}
		if _, dup := seen[iface.Name]; dup {
			return fmt.Errorf("network.interfaces[%d]: duplicate name %q", i, iface.Name)
		}
		seen[iface.Name] = struct{}{}
	}

	for i, m := range c.Mounts {
		if m.Device == "" || m.Path == "" || m.FSType == "" {
			return fmt.Errorf("mounts[%d]: device, path and fstype are all required", i)
		}
	}

	if o := c.OverlayRoot; o != nil {
		if o.Lower.Device == "" || o.Lower.FSType == "" {
			return fmt.Errorf("overlay_root.lower: device and fstype are required")
		}
		if o.Upper.Device == "" {
			return fmt.Errorf("overlay_root.upper: device is required")
		}
	}

	return nil
}
