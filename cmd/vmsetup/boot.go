package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/0ploy/vmsetup/internal/config"
	"github.com/0ploy/vmsetup/internal/identity"
	"github.com/0ploy/vmsetup/internal/machine"
	"github.com/0ploy/vmsetup/internal/mounts"
	"github.com/0ploy/vmsetup/internal/netconf"
	"github.com/0ploy/vmsetup/internal/userenv"
	"github.com/0ploy/vmsetup/internal/vmlog"
)

// Park steps, SPEC.md §7. fctl surfaces them, so they are protocol.
const (
	stepConfig   = "config"
	stepRoot     = "root"
	stepMounts   = "mounts"
	stepNetwork  = "network"
	stepIdentity = "identity"
	stepUser     = "user"
	stepHandoff  = "handoff"
)

// The init bundle's layout, SPEC.md §10.2.
const (
	bundleZpinit = "/bin/zpinit"
	bundleZpctl  = "/bin/zpctl"
)

// bins are what survive the switch root, onto machine.BinDir.
var bins = []mounts.Binary{
	{Src: bundleZpinit, Name: "zpinit"},
	{Src: bundleZpctl, Name: "zpctl"},
}

// rescueStop is the stop budget a parked machine gets when the config could
// not be read. zpinit only idles in rescue, so it has nothing to stop.
var rescueStop = machine.Stop{Signal: "SIGTERM", Timeout: config.Duration(10 * time.Second)}

// setup is vmsetup's state for the few milliseconds it is PID 1.
type setup struct {
	log     *vmlog.Logger
	console *os.File
	cfg     *config.Config // nil until the config is read
	env     *userenv.Env   // nil until composed

	switched bool // the VM's disk is the root
	parking  bool // park has started; a second failure gives up
}

// boot is vmsetup's entire life: bring the machine up, then execve zpinit.
// It never returns and never manages a process. Every path ends in execve,
// or, when there is no zpinit to hand the machine to, in reboot. PID 1
// returning or exiting would panic the kernel instead.
func boot() {
	s := &setup{}

	// First, before anything can go wrong: catch every signal and drop it.
	// For a terminating signal with no Notify, the Go runtime ends the
	// process, which for PID 1 is a kernel panic. execve resets every
	// disposition, so nothing of this reaches zpinit.
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, catchable()...)
	go func() {
		for range sigs {
		}
	}()

	s.log = vmlog.New()

	// /dev, the console and /proc. Nothing here depends on the config.
	if err := mounts.Dev(s.log); err != nil {
		s.park(stepConfig, err)
	}
	s.console = s.log.Attach(string(config.LogSerial))
	s.setupStdio()
	if err := mounts.Proc(s.log); err != nil {
		s.park(stepConfig, err)
	}

	cfg, err := loadConfig()
	if err != nil {
		s.park(stepConfig, err)
	}
	s.cfg = cfg
	if cfg.Logging.Target == config.LogNone {
		s.log.Discard()
		s.console = nil
		s.setupStdio()
	}
	if cfg.Handoff.Wrap() {
		s.log.Infof("vmsetup %s: zpinit wrap mode, command %q as %s", version, cfg.Handoff.Command, cfg.Handoff.User)
	} else {
		s.log.Infof("vmsetup %s: zpinit supervise mode, the image's own services", version)
	}

	// A bundle without zpinit has nothing to hand the machine to. Say so
	// before the switch, which would otherwise fail copying it and blame
	// the root.
	if _, err := os.Stat(bundleZpinit); err != nil {
		s.park(stepHandoff, fmt.Errorf("the init bundle has no zpinit: %w", err))
	}
	if err := mounts.SwitchRoot(s.log, cfg.Root, machine.BinDir, bins); err != nil {
		s.park(stepRoot, err)
	}
	s.switched = true

	if step, err := s.bringup(); err != nil {
		s.park(step, err)
	}
	s.handoff()
}

func loadConfig() (*config.Config, error) {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, fmt.Errorf("read /proc/cmdline: %w", err)
	}
	dev, err := config.DeviceFromCmdline(string(cmdline))
	if err != nil {
		return nil, err
	}
	doc, err := config.ReadBlob(dev)
	if err != nil {
		return nil, err
	}
	return config.Parse(doc)
}

// bringup prepares the new root (SPEC.md §5) and writes the handoff files (§6). It returns the step that failed.
func (s *setup) bringup() (string, error) {
	cfg := s.cfg

	if err := mounts.Standard(s.log); err != nil {
		return stepMounts, err
	}
	if err := mounts.Extra(s.log, cfg.Mounts); err != nil {
		return stepMounts, err
	}
	if err := netconf.Apply(s.log, cfg.Network); err != nil {
		return stepNetwork, err
	}
	if err := identity.Apply(s.log, cfg); err != nil {
		return stepIdentity, err
	}

	// zpinit resolves these names again when it switches to them. Resolving
	// them here makes a typo park the VM at boot, naming the field, instead
	// of surfacing later as a failed child.
	home := "/root"
	if cfg.Handoff.Wrap() {
		u, err := userenv.Resolve(cfg.Handoff.User)
		if err != nil {
			return stepUser, fmt.Errorf("handoff.user: %w", err)
		}
		s.log.Infof("resolved handoff.user %s to uid=%d gid=%d home=%s", cfg.Handoff.User, u.UID, u.GID, u.Home)
		home = u.Home
	} else if u, err := userenv.Resolve("root"); err == nil {
		home = u.Home
	}
	if e := cfg.Exec; e != nil {
		if _, err := userenv.Resolve(e.User); err != nil {
			return stepUser, fmt.Errorf("exec.user: %w", err)
		}
	}

	s.env = userenv.Compose(cfg.Env)
	s.env.ApplyDefaults(&userenv.User{Home: home})

	skipped, err := machine.WriteEnv(s.env.Slice())
	if err != nil {
		return stepHandoff, err
	}
	for _, k := range skipped {
		s.log.Warnf("env variable %q cannot be represented in %s; it reaches zpinit through envp only", k, machine.EnvPath)
	}
	if err := machine.FromConfig(cfg, vmlog.SerialPath, s.env.Slice()).Write(); err != nil {
		return stepHandoff, err
	}
	if cfg.Exec != nil {
		s.log.Infof("exec: zpinit will listen on vsock port %d", cfg.Exec.Port)
	}
	return "", nil
}

// handoff execs zpinit (SPEC.md §6). It returns only by parking.
func (s *setup) handoff() {
	h := s.cfg.Handoff
	path := machine.BinDir + "/zpinit"
	argv := []string{path}
	dir := "/"
	if h.Wrap() {
		argv = append(append(argv, "--"), h.Command...)
		dir = h.WorkingDir
	}
	if err := unix.Chdir(dir); err != nil {
		s.park(stepHandoff, fmt.Errorf("chdir %s: %w", dir, err))
	}
	s.log.Infof("bringup complete in %.3fms; exec %s", float64(s.log.Elapsed().Microseconds())/1000, path)
	err := unix.Exec(path, argv, s.env.Slice())
	s.park(stepHandoff, fmt.Errorf("execve %s: %w", path, err))
}

// park reports a bringup failure and hands the broken machine to zpinit's
// rescue mode, where it stays up to be inspected (SPEC.md §7). It never
// returns.
func (s *setup) park(step string, err error) {
	if !s.log.Attached() {
		// Failed before the console was attached; the error has to reach
		// the host regardless.
		s.console = s.log.Attach(string(config.LogSerial))
	}
	s.log.Errorf("bringup failed at %s: %v", step, err)
	if s.parking {
		s.giveUp(err)
	}
	s.parking = true
	// Quoted, so the line stays one line whatever the error says.
	vmlog.Sentinel(fmt.Sprintf("vmsetup: parked step=%s error=%s", step, strconv.Quote(err.Error())))

	if err := mounts.EnsureRun(s.log); err != nil {
		s.giveUp(fmt.Errorf("no /run for machine.json: %w", err))
	}
	settings := &machine.Settings{
		V:       machine.Version,
		Console: vmlog.SerialPath,
		Stop:    rescueStop,
		Rescue:  &machine.Rescue{Step: step, Error: err.Error()},
	}
	var env []string
	if s.cfg != nil {
		settings.Stop = machine.Stop{Signal: s.cfg.Handoff.StopSignal, Timeout: s.cfg.Handoff.StopTimeout}
		env = s.cfg.Env
		if s.env != nil {
			env = s.env.Slice()
		}
		if e := s.cfg.Exec; e != nil {
			settings.Exec = &machine.Exec{Port: e.Port, User: e.User, WorkingDir: e.WorkingDir, Env: env}
		}
	}
	if err := settings.Write(); err != nil {
		s.giveUp(err)
	}
	if err := os.MkdirAll(machine.RescueConfigDir, 0o755); err != nil {
		s.giveUp(err)
	}

	path := bundleZpinit
	if s.switched {
		path = machine.BinDir + "/zpinit"
	}
	_ = unix.Chdir("/")
	s.log.Infof("parked; exec %s in rescue mode", path)
	argv := []string{path, "--skip-entrypoint", "--config", machine.RescueConfigDir}
	err = unix.Exec(path, argv, env)
	s.giveUp(fmt.Errorf("execve %s: %w", path, err))
}

// giveUp ends the VM when there is no zpinit to hand it to: a bundle built
// wrong, a host-side bug. vmsetup does not stay to reap or wait for a stop,
// because that would be process management (SPEC.md §7).
func (s *setup) giveUp(err error) {
	s.log.Errorf("cannot hand the machine to zpinit: %v; rebooting", err)
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
		s.log.Errorf("reboot: %v", err)
	}
	// Reboot does not return on success. If it failed there is nothing
	// left; PID 1 must still not exit.
	for {
		time.Sleep(time.Hour)
	}
}

// setupStdio points PID 1's stdio somewhere real (SPEC.md §5): fd 0 at
// /dev/null, fds 1 and 2 at the console, or /dev/null without one. Production
// boots without console=, so the kernel may hand init closed descriptors, and
// a process inheriting a closed fd 1 gets it assigned to the first file it
// opens. zpinit inherits what is set here across the execve.
func (s *setup) setupStdio() {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		s.log.Warnf("could not open %s: %v", os.DevNull, err)
		return
	}
	defer null.Close()

	out := null
	if s.console != nil {
		out = s.console
	}
	for fd, src := range []*os.File{null, out, out} {
		if err := unix.Dup2(int(src.Fd()), fd); err != nil {
			s.log.Warnf("could not redirect fd %d: %v", fd, err)
		}
	}
}

// catchable is every signal Notify can take, except SIGURG, which the Go
// runtime uses for goroutine preemption and would only be noise here.
func catchable() []os.Signal {
	var out []os.Signal
	for sig := syscall.Signal(1); sig <= 31; sig++ {
		switch sig {
		case syscall.SIGKILL, syscall.SIGSTOP, syscall.SIGURG:
			continue
		}
		out = append(out, sig)
	}
	return out
}
