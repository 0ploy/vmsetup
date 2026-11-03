// Package mounts brings up the guest's filesystem hierarchy: devtmpfs and
// /proc in the initramfs, the switch into the VM's disk, then the standard
// table and the data volumes. See SPEC.md §5.
//
// The table's ordering mirrors fly.io's init
// (https://github.com/superfly/init-snapshot, src/bin/init/main.rs). Two deliberate departures: cgroup2 is mounted unified at
// /sys/fs/cgroup with no tmpfs wrapper and no v1 controller hierarchy, and
// hugetlbfs is left out, because both exist for legacy workloads that modern
// images do not need.
package mounts

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/0ploy/vmsetup/internal/config"
	"github.com/0ploy/vmsetup/internal/devwait"
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

var (
	// dev and proc are mounted in the initramfs, before the config is
	// read, and moved into the new root by SwitchRoot.
	dev  = spec{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755", 0o755}
	proc = spec{"proc", "/proc", "proc", common, "", 0o555}
	// run is mounted during the switch, because the bin tmpfs lives inside
	// it, and in the initramfs itself when a parked machine never switched.
	run = spec{"run", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755", 0o755}
)

// standard is the rest of the fixed mount table, in order, mounted after the
// switch. /dev, /proc and /run are already there by then.
var standard = []spec{
	{"sysfs", "/sys", "sysfs", common, "", 0o555},
	{"devpts", "/dev/pts", "devpts", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NOATIME, "mode=0620,gid=5,ptmxmode=666", 0o755},
	{"shm", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777", 0o1777},
	{"mqueue", "/dev/mqueue", "mqueue", common, "", 0o755},
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

// NewRoot is where the VM's disk is mounted before the switch.
const NewRoot = "/newroot"

// Logger is the subset of vmlog.Logger this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Dev mounts devtmpfs on /dev, vmsetup's very first act.
//
// In an initramfs the kernel does not do this: CONFIG_DEVTMPFS_MOUNT acts
// only when the kernel mounts a root device itself. The console and the
// config device both live in /dev, so nothing can be reported or read before
// this. Nothing about it depends on the config.
func Dev(log Logger) error { return mountOne(log, dev) }

// Proc mounts /proc in the initramfs, so /proc/cmdline can be read before the
// config is known (SPEC.md §5).
func Proc(log Logger) error { return mountOne(log, proc) }

// EnsureRun makes /run a tmpfs in whatever root vmsetup is in, unless it
// already is a mount. A parked machine needs it for machine.json whether or
// not the switch happened.
func EnsureRun(log Logger) error {
	if isMountpoint(run.target) {
		return nil
	}
	return mountOne(log, run)
}

// Binary is one file the bundle carries that must survive the switch root.
type Binary struct {
	Src  string // path in the initramfs, e.g. /bin/zpinit
	Name string // file name under binDir
}

// SwitchRoot mounts the VM's disk and makes it the root (SPEC.md §5):
//
//	mount root on /newroot; check it is writable
//	mount /newroot/run, then a tmpfs at /newroot<binDir>; copy bins in; remount it ro
//	MS_MOVE /dev, /proc into /newroot
//	delete the initramfs's own files, not crossing into other filesystems
//	chdir /newroot; MS_MOVE . onto /; chroot .; chdir /
//
// pivot_root does not work here: the initramfs is the rootfs mount itself,
// which cannot be unmounted, so pivot_root(2) refuses it. MS_MOVE plus
// chroot is what busybox's and klibc's switch_root do.
//
// On error the machine has not switched: the caller parks in the initramfs.
func SwitchRoot(log Logger, root config.Root, binDir string, bins []Binary) error {
	if err := checkInitramfs(); err != nil {
		return err
	}

	if err := devwait.Device(root.Device, devwait.Timeout); err != nil {
		return fmt.Errorf("root: %w", err)
	}
	flags, data := parseOptions(root.Options)
	if err := os.MkdirAll(NewRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", NewRoot, err)
	}
	if err := unix.Mount(root.Device, NewRoot, root.FSType, flags, data); err != nil {
		return fmt.Errorf("mount root %s (%s) on %s: %w", root.Device, root.FSType, NewRoot, err)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(NewRoot, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", NewRoot, err)
	}
	if st.Flags&unix.ST_RDONLY != 0 {
		return fmt.Errorf("root %s mounted read-only; it must be the VM's writable disk", root.Device)
	}
	log.Infof("mounted root %s (%s) on %s", root.Device, root.FSType, NewRoot)

	// The disk is writable, so the mount points an OCI image lacks can be
	// created here; nothing has to pre-create them.
	for _, d := range []string{"dev", "proc", "sys", "run"} {
		if err := os.MkdirAll(filepath.Join(NewRoot, d), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Join(NewRoot, d), err)
		}
	}
	newRun := run
	newRun.target = filepath.Join(NewRoot, run.target)
	if err := mountOne(log, newRun); err != nil {
		return err
	}
	if err := installBins(filepath.Join(NewRoot, binDir), bins); err != nil {
		return err
	}

	for _, m := range []string{dev.target, proc.target} {
		if err := unix.Mount(m, filepath.Join(NewRoot, m), "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move %s into the new root: %w", m, err)
		}
	}

	freed, err := freeInitramfs()
	if err != nil {
		// Not fatal: the switch still works, the files only keep costing
		// guest RAM.
		log.Warnf("could not fully free the initramfs: %v", err)
	}
	log.Infof("freed %d KiB of initramfs", freed>>10)

	if err := unix.Chdir(NewRoot); err != nil {
		return fmt.Errorf("chdir %s: %w", NewRoot, err)
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move the new root onto /: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	return nil
}

// checkInitramfs refuses to switch from anything but an initramfs. Freeing
// the old root deletes every file on it, so running this on a disk root,
// say vmsetup booted by hand with init=, must fail here rather than wipe it.
func checkInitramfs() error {
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil {
		return fmt.Errorf("statfs /: %w", err)
	}
	switch st.Type {
	case unix.RAMFS_MAGIC, unix.TMPFS_MAGIC:
		return nil
	}
	return fmt.Errorf("/ is not an initramfs (filesystem magic %#x); vmsetup must run as /init of the init bundle", st.Type)
}

// installBins puts the bundle's binaries on a tmpfs of their own at dir, then
// makes it read-only. They have to survive the switch, after which the
// initramfs is unreachable, and they must come from the host rather than from
// the workload's disk (SPEC.md §11).
func installBins(dir string, bins []Binary) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := unix.Mount("zpinit-bin", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mount tmpfs on %s: %w", dir, err)
	}
	for _, b := range bins {
		if err := copyFile(b.Src, filepath.Join(dir, b.Name), 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount("", dir, "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("remount %s read-only: %w", dir, err)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return out.Close()
}

// freeInitramfs deletes the initramfs's files, which would otherwise stay in
// guest RAM for the life of the VM, without crossing into any other
// filesystem -- the new root and the moved mounts above all. It returns the
// bytes freed.
func freeInitramfs() (int64, error) {
	var st unix.Stat_t
	if err := unix.Lstat("/", &st); err != nil {
		return 0, err
	}
	var freed int64
	var errs []error
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, err)
			return
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			var est unix.Stat_t
			if err := unix.Lstat(p, &est); err != nil {
				errs = append(errs, err)
				continue
			}
			if est.Dev != st.Dev {
				continue // a mount point: another filesystem
			}
			if e.IsDir() {
				walk(p)
				if err := unix.Rmdir(p); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			if err := unix.Unlink(p); err != nil {
				errs = append(errs, err)
				continue
			}
			freed += est.Size
		}
	}
	walk("/")
	return freed, errors.Join(errs...)
}

// Standard re-flags the moved /dev and /proc, mounts the rest of the fixed
// table and creates the /dev symlinks. A failure is fatal: nothing downstream
// works without these, and a machine that boots with no /sys is worse than
// one that refuses to.
func Standard(log Logger) error {
	// /dev and /proc were moved, not mounted, here; the table's row for
	// them means "apply these flags". MS_REMOUNT|MS_BIND changes the
	// per-mount flags without touching the superblock, which matters for
	// devtmpfs in particular, being a single kernel-wide instance.
	for _, s := range []spec{dev, proc} {
		if err := unix.Mount("", s.target, "", unix.MS_REMOUNT|unix.MS_BIND|s.flags, ""); err != nil {
			log.Warnf("%s: could not apply its flags (%v); it keeps the flags it was mounted with", s.target, err)
		}
	}
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
	// second one would only hide the first. Booted from a root drive, a
	// kernel with CONFIG_DEVTMPFS_MOUNT=y mounts devtmpfs before init runs,
	// with none of the flags below. Silently accepting that loses nosuid on
	// /dev [VERIFIED], so adopt the mount and apply the flags to it.
	if s.flags != 0 {
		if err := unix.Mount("", s.target, "", unix.MS_REMOUNT|unix.MS_BIND|s.flags, ""); err != nil {
			log.Warnf("%s was already mounted and its flags could not be tightened (%v); it keeps the flags it was mounted with", s.target, err)
		}
	}
	return nil
}

// isMountpoint reports whether p is the root of a mount: its device differs
// from its parent's. Good enough for the tmpfs and block mounts vmsetup
// itself makes; it does not detect a bind mount of a directory onto its own
// filesystem, which nothing here creates.
func isMountpoint(p string) bool {
	var st, parent unix.Stat_t
	if unix.Lstat(p, &st) != nil || unix.Lstat(filepath.Dir(p), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// Extra applies the config's data volumes, after the standard table.
func Extra(log Logger, ms []config.Mount) error {
	for _, m := range ms {
		if err := devwait.Device(m.Device, devwait.Timeout); err != nil {
			return fmt.Errorf("mount on %s: %w", m.Path, err)
		}
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
