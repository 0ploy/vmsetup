// Package mounts brings up the guest's filesystem hierarchy. See PLAN.md
// §5.2. The ordering mirrors fly.io's init (fly-io-init/src/bin/init/main.rs
// lines 187-330), which is the closest prior art and has been in production
// far longer than this has.
//
// Two deliberate departures from that reference: cgroup2 is mounted unified
// at /sys/fs/cgroup with no tmpfs wrapper and no v1 controller hierarchy,
// because the v1 tree exists for legacy workloads that modern images do not
// need; and hugetlbfs is left out for the same reason.
package mounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/0ploy/vminit/internal/config"
)

// common is the flag set every pseudo-filesystem gets: no devices, no
// executables, no setuid. None of these filesystems has any business
// carrying any of the three.
const common = unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOSUID

type spec struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
	// dirMode is the mode the mountpoint is created with when missing.
	dirMode os.FileMode
}

// dev is mounted on its own, ahead of everything else, because the console
// lives in it: a rootfs exported from a Docker image contains no device nodes
// at all, so until devtmpfs is up nothing vminit says can reach the host. See
// Dev.
var dev = spec{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755", 0o755}

// standard is the rest of the fixed mount table, in order.
var standard = []spec{
	{"proc", "/proc", "proc", common, "", 0o555},
	{"sysfs", "/sys", "sysfs", common, "", 0o555},
	{"devpts", "/dev/pts", "devpts", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NOATIME, "mode=0620,gid=5,ptmxmode=666", 0o755},
	{"shm", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777", 0o1777},
	{"mqueue", "/dev/mqueue", "mqueue", common, "", 0o755},
	{"run", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755", 0o755},
	{"cgroup2", "/sys/fs/cgroup", "cgroup2", common | unix.MS_RELATIME, "", 0o555},
}

// symlinks are the /dev entries a devtmpfs does not provide but that a great
// deal of software assumes. Created after /proc, since three of the four
// point into it.
var symlinks = []struct{ target, link string }{
	{"/proc/self/fd", "/dev/fd"},
	{"/proc/self/fd/0", "/dev/stdin"},
	{"/proc/self/fd/1", "/dev/stdout"},
	{"/proc/self/fd/2", "/dev/stderr"},
}

// Logger is the subset of vmlog.Logger this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Dev mounts devtmpfs on /dev and nothing else.
//
// It is separate from Standard so that the console can be opened before the
// configuration is read. Reading the config is the step most likely to fail,
// and a failure there has to be visible on the host serial log, which means
// /dev/ttyS0 has to exist by then. Nothing about mounting /dev depends on the
// config, so the ordering costs nothing.
//
// When an overlay root is introduced (PLAN.md §5.2) this mount has to be
// MS_MOVE'd into the merged root rather than repeated, the way
// fly-io-init/src/bin/init/main.rs:214 does it.
func Dev(log Logger) error { return mountOne(log, dev) }

// Standard mounts the rest of the fixed table and creates the /dev symlinks.
// A failure is fatal: nothing downstream works without these, and a machine
// that boots with no /proc is worse than one that refuses to.
func Standard(log Logger) error {
	for _, s := range standard {
		if err := mountOne(log, s); err != nil {
			return err
		}
	}

	// devtmpfs does provide /dev/ptmx, but a kernel without
	// CONFIG_LEGACY_PTYS or with a different devpts setup may not, and
	// glibc opens /dev/ptmx rather than /dev/pts/ptmx.
	if _, err := os.Lstat("/dev/ptmx"); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink("/dev/pts/ptmx", "/dev/ptmx"); err != nil {
			log.Warnf("could not create /dev/ptmx symlink: %v", err)
		}
	}

	for _, s := range symlinks {
		if err := os.Symlink(s.target, s.link); err != nil && !errors.Is(err, os.ErrExist) {
			log.Warnf("could not link %s -> %s: %v", s.link, s.target, err)
		}
	}
	return nil
}

func mountOne(log Logger, s spec) error {
	if err := os.MkdirAll(s.target, s.dirMode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("mkdir %s: %w", s.target, err)
	}
	err := unix.Mount(s.source, s.target, s.fstype, s.flags, s.data)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("mount %s on %s: %w", s.fstype, s.target, err)
	}

	// EBUSY means this filesystem is already mounted here, so stacking a
	// second one would only hide the first. That happens for /dev on every
	// boot: a kernel built with CONFIG_DEVTMPFS_MOUNT=y mounts devtmpfs
	// before it execs init, and it does so with none of the flags below.
	// Silently accepting that loses nosuid on /dev, so adopt the existing
	// mount instead and apply the flags to it.
	//
	// MS_REMOUNT|MS_BIND changes the per-mount flags only, leaving the
	// superblock alone. That matters for devtmpfs in particular, which is a
	// single kernel-wide instance rather than one filesystem per mount.
	if s.flags != 0 {
		if err := unix.Mount("", s.target, "", unix.MS_REMOUNT|unix.MS_BIND|s.flags, ""); err != nil {
			log.Warnf("%s was already mounted and its flags could not be tightened (%v); it keeps the flags it was mounted with", s.target, err)
		}
	}
	return nil
}

// RemountRootRW makes / writable when the kernel mounted it read-only.
//
// This is needed more often than it looks: the kernel mounts the root
// filesystem read-only unless `rw` appears on the command line, and the boot
// args in circulation here do not always carry it. Everything in PLAN.md §5.4
// writes to /etc, so without this the first identity file fails.
//
// Non-fatal by itself. If the drive really is read-only the later writes fail
// loudly with a message that names the file, which is more useful than
// failing here with a message about mount flags.
func RemountRootRW(log Logger) {
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil {
		log.Warnf("cannot statfs /: %v", err)
		return
	}
	if st.Flags&unix.ST_RDONLY == 0 {
		return
	}
	if err := unix.Mount("", "/", "", unix.MS_REMOUNT, ""); err != nil {
		log.Warnf("/ is mounted read-only and cannot be remounted read-write (%v); writes to /etc will fail", err)
		return
	}
	log.Infof("remounted / read-write")
}

// Extra applies the config's data volumes, after the standard table.
func Extra(log Logger, ms []config.Mount) error {
	for _, m := range ms {
		flags, data := parseOptions(m.Options)
		if err := os.MkdirAll(m.Path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("mkdir %s: %w", m.Path, err)
		}
		if err := unix.Mount(m.Device, m.Path, m.FSType, flags, data); err != nil {
			return fmt.Errorf("mount %s on %s: %w", m.Device, m.Path, err)
		}
		log.Infof("mounted %s on %s (%s)", m.Device, m.Path, m.FSType)
	}
	return nil
}

// flagOptions maps the mount options that are kernel flags rather than
// filesystem-specific data. Anything not listed here is passed through to the
// filesystem untouched, which is what makes options like `data=ordered` or
// `subvol=` work without this package knowing about them.
var flagOptions = map[string]uintptr{
	"ro":          unix.MS_RDONLY,
	"nosuid":      unix.MS_NOSUID,
	"nodev":       unix.MS_NODEV,
	"noexec":      unix.MS_NOEXEC,
	"sync":        unix.MS_SYNCHRONOUS,
	"dirsync":     unix.MS_DIRSYNC,
	"noatime":     unix.MS_NOATIME,
	"nodiratime":  unix.MS_NODIRATIME,
	"relatime":    unix.MS_RELATIME,
	"strictatime": unix.MS_STRICTATIME,
	"lazytime":    unix.MS_LAZYTIME,
	"mand":        unix.MS_MANDLOCK,
	"nosymfollow": unix.MS_NOSYMFOLLOW,
}

// clearOptions are the negative forms. They have no kernel flag of their own;
// they exist so a config can cancel a flag another option set.
var clearOptions = map[string]uintptr{
	"rw":       unix.MS_RDONLY,
	"suid":     unix.MS_NOSUID,
	"dev":      unix.MS_NODEV,
	"exec":     unix.MS_NOEXEC,
	"async":    unix.MS_SYNCHRONOUS,
	"atime":    unix.MS_NOATIME,
	"diratime": unix.MS_NODIRATIME,
	"nomand":   unix.MS_MANDLOCK,
}

func parseOptions(opts string) (uintptr, string) {
	var flags uintptr
	var data []string
	for _, opt := range strings.Split(opts, ",") {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		if f, ok := flagOptions[opt]; ok {
			flags |= f
			continue
		}
		if f, ok := clearOptions[opt]; ok {
			flags &^= f
			continue
		}
		data = append(data, opt)
	}
	return flags, strings.Join(data, ",")
}

// UnmountExtra unmounts the config's data volumes on shutdown, deepest path
// first so a volume mounted inside another comes off before its parent.
// Best effort with a lazy fallback: a busy mount must not stop the machine
// from going down, and the writes have already been flushed by the caller's
// sync.
func UnmountExtra(log Logger, ms []config.Mount) {
	paths := make([]string, 0, len(ms))
	for _, m := range ms {
		paths = append(paths, filepath.Clean(m.Path))
	}
	// Deepest first: more separators means deeper.
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if strings.Count(paths[j], "/") > strings.Count(paths[i], "/") {
				paths[i], paths[j] = paths[j], paths[i]
			}
		}
	}
	for _, p := range paths {
		if err := unix.Unmount(p, 0); err == nil {
			continue
		}
		if err := unix.Unmount(p, unix.MNT_DETACH); err != nil {
			log.Warnf("could not unmount %s: %v", p, err)
		}
	}
}
