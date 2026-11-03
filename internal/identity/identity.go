// Package identity writes the three files a container runtime would have
// written for you, and sets the hostname. See SPEC.md §5.
//
// Docker's export leaves /etc/resolv.conf, /etc/hosts and /etc/hostname as
// 0-byte regular files even for a `scratch` image, because the runtime
// bind-mounts over them at run time. Nothing bind-mounts anything here, so
// they have to be filled in, and truncated rather than appended to so that
// re-running is idempotent.
package identity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/0ploy/vmsetup/internal/config"
)

// Logger is the subset of vmlog.Logger this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Apply sets the hostname and writes /etc/hostname, /etc/hosts and
// /etc/resolv.conf.
func Apply(log Logger, cfg *config.Config) error {
	if err := os.MkdirAll("/etc", 0o755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("mkdir /etc: %w", err)
	}

	if cfg.Hostname != "" {
		if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
			return fmt.Errorf("sethostname %q: %w", cfg.Hostname, err)
		}
		if err := write("/etc/hostname", cfg.Hostname+"\n"); err != nil {
			return err
		}
		log.Infof("hostname %s", cfg.Hostname)
	}

	if err := write("/etc/hosts", hostsFile(cfg)); err != nil {
		return err
	}
	if resolv := resolvConf(cfg.Network); resolv != "" {
		if err := write("/etc/resolv.conf", resolv); err != nil {
			return err
		}
	} else {
		log.Warnf("no nameservers configured; DNS resolution will not work in the guest")
	}
	return nil
}

func hostsFile(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")

	// The VM's own address maps to its hostname, which is what makes
	// `ping $(hostname)` and any software that resolves its own name work.
	if cfg.Hostname != "" {
		for _, iface := range cfg.Network.Interfaces {
			ip, _, found := strings.Cut(iface.Address, "/")
			if !found || ip == "" {
				continue
			}
			fmt.Fprintf(&b, "%s\t%s\n", ip, cfg.Hostname)
			break
		}
	}

	for _, h := range cfg.Network.ExtraHosts {
		if h.IP == "" || h.Host == "" {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\n", h.IP, h.Host)
	}
	return b.String()
}

func resolvConf(nc config.Network) string {
	if len(nc.Nameservers) == 0 {
		return ""
	}
	var b strings.Builder
	if len(nc.Search) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(nc.Search, " "))
	}
	for _, ns := range nc.Nameservers {
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}
	return b.String()
}

// write replaces a file atomically, following no symlinks.
//
// The rename is what makes it work on the files Docker leaves behind and on a
// /etc/resolv.conf that some images ship as a symlink: the link is replaced
// rather than written through. O_NOFOLLOW on the temporary file keeps a
// symlink planted in /etc from redirecting the write, which matters less than
// it would on a host -- the tenant owns this rootfs either way -- but costs
// nothing.
func write(path, content string) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".vmsetup")
	_ = os.Remove(tmp)

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
