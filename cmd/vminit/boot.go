package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/0ploy/vminit/internal/config"
	"github.com/0ploy/vminit/internal/identity"
	"github.com/0ploy/vminit/internal/mounts"
	"github.com/0ploy/vminit/internal/netconf"
	"github.com/0ploy/vminit/internal/userenv"
	"github.com/0ploy/vminit/internal/vmlog"
)

type booter struct {
	log          *vmlog.Logger
	basePath     string
	overridePath string
	console      *os.File
}

// run performs bringup in order and hands off. It returns only on failure:
// the zpinit profile ends in execve, so a successful run never comes back.
//
// The order differs from PLAN.md §5 in one place, deliberately. The plan reads
// config first and mounts second; here devtmpfs is mounted first so that the
// console exists before the config is parsed. Reading the config is the step
// most likely to fail and the failure has to be visible on the host, which it
// is not if /dev/ttyS0 does not exist yet.
func (b *booter) run() error {
	// The kernel mounts / read-only unless `rw` is on the command line, and
	// every write below lands in /etc. Do this before the first mkdir.
	mounts.RemountRootRW(b.log)

	if err := mounts.Dev(b.log); err != nil {
		return err
	}

	// Provisional console at the default target, so config errors are
	// reportable. Reconciled with logging.target once the config is known.
	b.console = b.log.Attach(string(config.LogSerial))
	b.setupStdio()

	cfg, err := config.Load(b.basePath, b.overridePath)
	if err != nil {
		return err
	}
	if cfg.Logging.Target == config.LogNone {
		b.log.Discard()
		b.console = nil
		b.setupStdio()
	}
	b.log.Infof("vminit %s starting: profile=%s", version, cfg.Boot.Profile)

	if cfg.OverlayRoot != nil {
		return fmt.Errorf("overlay_root is configured but not implemented yet (PLAN.md §5.2); " +
			"refusing to boot rather than silently ignoring a shared read-only lower layer")
	}
	if !cfg.Boot.Profile.Apply() {
		return fmt.Errorf("boot.profile %q is not implemented yet (PLAN.md §4.4); only zpinit is supported so far", cfg.Boot.Profile)
	}
	if cfg.Boot.Profile == config.ProfileCommand {
		return fmt.Errorf("boot.profile %q is not implemented yet (PLAN.md §5.6, milestone M2); only zpinit is supported so far", cfg.Boot.Profile)
	}

	if err := mounts.Standard(b.log); err != nil {
		return err
	}
	if err := mounts.Extra(b.log, cfg.Mounts); err != nil {
		return err
	}

	if err := netconf.Apply(b.log, cfg.Network, cfg.MMDS.Address); err != nil {
		return err
	}
	if err := identity.Apply(b.log, cfg); err != nil {
		return err
	}

	user, err := userenv.Resolve(cfg.User())
	if err != nil {
		return err
	}
	b.log.Infof("resolved user %s to uid=%d gid=%d home=%s", cfg.User(), user.UID, user.GID, user.Home)

	env := userenv.Compose(cfg, nil)
	env.ApplyDefaults(user)

	// vminit's own PATH has to match the workload's, because it is what
	// resolves the handoff target's argv[0] when the config gives a bare
	// name rather than an absolute path.
	if path, ok := env.Get("PATH"); ok {
		if err := os.Setenv("PATH", path); err != nil {
			b.log.Warnf("could not set PATH: %v", err)
		}
	}

	return b.handoffZpinit(cfg, env)
}

// setupStdio points PID 1's stdio somewhere real before anything is exec'd.
//
// The kernel gives init whatever /dev/console resolved to, which in production
// is nothing at all: the VM boots without console= on the command line
// (PLAN.md §7.2), so fd 0, 1 and 2 may be closed. A child inheriting closed
// descriptors is a well-known way to get corrupted output, because the first
// file it opens becomes fd 1. zpinit in particular logs to os.Stderr, so
// without this its output is lost.
func (b *booter) setupStdio() {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		b.log.Warnf("could not open %s: %v", os.DevNull, err)
		return
	}
	defer null.Close()

	out := null
	if b.console != nil {
		out = b.console
	}
	for fd, src := range []*os.File{null, out, out} {
		if err := unix.Dup2(int(src.Fd()), fd); err != nil {
			b.log.Warnf("could not redirect fd %d: %v", fd, err)
		}
	}
}
