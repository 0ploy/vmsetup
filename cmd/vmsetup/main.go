// Command vmsetup is the guest half of running an OCI image as a Firecracker
// microVM. It runs as /init of the init bundle, an initramfs fctl attaches on
// every boot: it mounts the VM's disk and switches root into it, performs the
// machine bringup a container runtime would normally do, and then execs
// zpinit, which is PID 1 for the life of the VM.
//
// It manages no process at any point: no fork, no reaping, no stop handling.
// See SPEC.md.
package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/0ploy/vmsetup/internal/config"
)

var version = "dev"

func main() {
	// As PID 1, argv is not ours to interpret: the kernel passes any bare
	// word it did not recognise on its command line as an argument to
	// /init. So PID 1 always boots, and the subcommands exist only for
	// running this by hand on a workstation.
	if os.Getpid() == 1 {
		boot()
	}
	os.Exit(cli(os.Args[1:]))
}

func cli(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "vmsetup runs as /init inside a microVM; by hand it supports:\n"+
			"  vmsetup version\n"+
			"  vmsetup check FILE   validate a config blob or a raw JSON document and print it")
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		return 0
	case "check":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: vmsetup check FILE")
			return 2
		}
		if err := check(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "vmsetup:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "vmsetup: unknown subcommand %q (want version or check)\n", args[0])
		return 2
	}
}

// check validates what fctl would hand the guest, off the PID 1 path. It
// accepts either a whole blob or the bare JSON document.
func check(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	doc := raw
	if bytes.HasPrefix(raw, []byte(config.Magic)) {
		if doc, err = config.Decode(bytes.NewReader(raw)); err != nil {
			return err
		}
	}
	cfg, err := config.Parse(doc)
	if err != nil {
		return err
	}
	printPlan(cfg)
	return nil
}

func printPlan(cfg *config.Config) {
	h := cfg.Handoff
	fmt.Printf("hostname     %s\n", cfg.Hostname)
	if h.Wrap() {
		fmt.Printf("handoff      zpinit wrap mode: %q as %s in %s\n", h.Command, h.User, h.WorkingDir)
	} else {
		fmt.Printf("handoff      zpinit supervise mode: the image's own services\n")
	}
	fmt.Printf("stop         %s, budget %s (zpinit's)\n", h.StopSignal, time.Duration(h.StopTimeout))
	fmt.Printf("root         %s (%s) %s\n", cfg.Root.Device, cfg.Root.FSType, cfg.Root.Options)
	fmt.Printf("env          %d variables\n", len(cfg.Env))
	for _, iface := range cfg.Network.Interfaces {
		mtu := "driver"
		if iface.MTU != 0 {
			mtu = fmt.Sprint(iface.MTU)
		}
		fmt.Printf("interface    %s %s gw=%s mac=%s mtu=%s mmds=%t\n",
			iface.Name, iface.Address, iface.Gateway, iface.MAC, mtu, iface.MMDS)
	}
	for _, ns := range cfg.Network.Nameservers {
		fmt.Printf("nameserver   %s\n", ns)
	}
	for _, m := range cfg.Mounts {
		fmt.Printf("mount        %s -> %s (%s) %s\n", m.Device, m.Path, m.FSType, m.Options)
	}
	if e := cfg.Exec; e != nil {
		fmt.Printf("exec         zpinit on vsock port %d, default user %s in %s\n", e.Port, e.User, e.WorkingDir)
	} else {
		fmt.Printf("exec         off\n")
	}
	fmt.Printf("logging      %s\n", cfg.Logging.Target)
	fmt.Printf("boot timer   %t\n", cfg.BootTimer)
}
