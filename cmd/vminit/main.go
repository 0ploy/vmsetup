// Command vminit is the guest half of running an OCI image as a Firecracker
// microVM. It runs as PID 1, performs the machine bringup a container runtime
// would normally do, and hands off to a supervisor.
//
// It is deliberately not a supervisor itself. In the normal case it execve's
// and ceases to exist. See PLAN.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/0ploy/vminit/internal/config"
	"github.com/0ploy/vminit/internal/vmlog"
)

var version = "dev"

func main() {
	// The kernel execs init= with no arguments at all, so a bare
	// invocation has to mean `boot`. Subcommands exist for the agent and
	// for running this by hand on a workstation.
	args := os.Args[1:]
	cmd := "boot"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "boot":
		runBoot(args)
	case "agent":
		fmt.Fprintln(os.Stderr, "vminit agent: not implemented yet (PLAN.md §6, milestone M5)")
		os.Exit(2)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "vminit: unknown subcommand %q (want boot, agent or version)\n", cmd)
		os.Exit(2)
	}
}

func isFlag(arg string) bool { return len(arg) > 1 && arg[0] == '-' }

func runBoot(args []string) {
	fs := flag.NewFlagSet("vminit boot", flag.ExitOnError)
	basePath := fs.String("config", config.BasePath, "build-time config `path`")
	overridePath := fs.String("override", config.OverridePath, "boot-time override `path`")
	dryRun := fs.Bool("dry-run", false, "load and print the resolved configuration, then exit without touching the machine")
	_ = fs.Parse(args)

	log := vmlog.New()

	if *dryRun {
		// Off the PID 1 path entirely: this runs on a workstation to check
		// what vmpack produced, so it logs to stderr and exits normally.
		log.Attach("none")
		cfg, err := config.Load(*basePath, *overridePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "vminit:", err)
			os.Exit(1)
		}
		printPlan(cfg)
		return
	}

	b := &booter{log: log, basePath: *basePath, overridePath: *overridePath}
	if err := b.run(); err != nil {
		b.fatal(err)
	}
}

// fatal reports a bringup failure and takes the machine down.
//
// Two things make this more than a call to os.Exit. The buffered log has to
// reach the console even when the failure happened before the console was
// opened, otherwise the host sees a VM that died silently. And PID 1 may not
// simply exit: that panics the kernel with "Attempted to kill init!" and
// leaves the VM hanging until something outside kills it, so the machine is
// rebooted instead, which makes Firecracker exit and gives the host a clean
// signal to pair with the log.
//
// The PID check matters: run by hand on a workstation this must not reboot
// the developer's machine.
func (b *booter) fatal(err error) {
	if !b.log.Attached() {
		b.log.Attach(string(config.LogSerial))
	}
	b.log.Errorf("bringup failed: %v", err)

	if os.Getpid() != 1 {
		os.Exit(1)
	}

	b.log.Errorf("PID 1 cannot exit; rebooting the VM")
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
		b.log.Errorf("reboot failed: %v", err)
		// Nothing left to try. Block rather than exit, because exiting is
		// the one outcome guaranteed to panic the kernel and hang the VM.
		for {
			time.Sleep(time.Hour)
		}
	}
}

func printPlan(cfg *config.Config) {
	fmt.Printf("profile      %s\n", cfg.Boot.Profile)
	switch cfg.Boot.Profile {
	case config.ProfileZpinit:
		fmt.Printf("handoff      execve %s %v\n", cfg.Boot.Path, cfg.Boot.Args)
	case config.ProfileCommand:
		fmt.Printf("handoff      fork %v\n", cfg.Argv())
	default:
		fmt.Printf("handoff      execve %s\n", cfg.Boot.Init)
	}
	fmt.Printf("hostname     %s\n", cfg.Hostname)
	fmt.Printf("user         %s\n", cfg.User())
	fmt.Printf("workdir      %s\n", cfg.Image.WorkingDir)
	fmt.Printf("logging      %s\n", cfg.Logging.Target)
	for _, iface := range cfg.Network.Interfaces {
		fmt.Printf("interface    %s %s gw=%s mac=%s mmds=%t\n",
			iface.Name, iface.Address, iface.Gateway, iface.MAC, iface.MMDS)
	}
	for _, ns := range cfg.Network.Nameservers {
		fmt.Printf("nameserver   %s\n", ns)
	}
	for _, m := range cfg.Mounts {
		fmt.Printf("mount        %s -> %s (%s) %s\n", m.Device, m.Path, m.FSType, m.Options)
	}
}
