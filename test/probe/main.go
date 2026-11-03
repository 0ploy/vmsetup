// Command vmprobe is the test payload for the boot matrix. It runs inside the
// guest -- as a zpinit service, or as zpinit's wrap-mode command -- and
// checks, from the inside, that vmsetup actually did what SPEC.md §5 and §6 say.
//
// Everything it needs to know about the expected machine arrives on the
// command line, so test/build-rootfs.sh can generate the guest config and
// this invocation from one set of values and the two cannot drift.
//
// Output goes to stdout, which is the serial console either way (vmsetup sets
// up PID 1's stdio, zpinit inherits it across the execve, and every child
// inherits it from zpinit), and Firecracker forwards it
// to the host. One line per check, then a summary
// line the host-side harness greps for. It is a test fixture, not part of the
// shipped binary.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type checker struct {
	pass, fail int
}

func (c *checker) check(name string, ok bool, detail string) {
	status := "ok"
	if !ok {
		status = "FAIL"
		c.fail++
	} else {
		c.pass++
	}
	fmt.Printf("PROBE %-4s %-28s %s\n", status, name, detail)
}

type envList []string

func (e *envList) String() string     { return strings.Join(*e, ",") }
func (e *envList) Set(v string) error { *e = append(*e, v); return nil }

func main() {
	// The harness also installs this binary as the image's own
	// /usr/local/bin/zpinit, a stand-in for a zpinit-native image. The
	// bundle's zpinit must be what runs as PID 1, so this must never run.
	if filepath.Base(os.Args[0]) == "zpinit" {
		fmt.Println("SHADOW-STUB-RAN: the image's own zpinit was executed")
		os.Exit(1)
	}
	var (
		hostname   = flag.String("hostname", "", "hostname vmsetup was told to set")
		address    = flag.String("address", "", "interface address in CIDR form")
		iface      = flag.String("interface", "eth0", "interface name")
		mtu        = flag.Int("mtu", 0, "expected MTU of the interface; 0 skips the check")
		gateway    = flag.String("gateway", "", "default gateway")
		nameserver = flag.String("nameserver", "", "expected nameserver in /etc/resolv.conf")
		uid        = flag.Int("uid", 0, "uid the service is expected to run as")
		gid        = flag.Int("gid", 0, "gid the service is expected to run as")
		serve      = flag.String("serve", "", "listen on this address and wait for the host to fetch /ping, proving the network works end to end")
		hold       = flag.Bool("hold", false, "sleep forever after reporting instead of exiting")
		ignoreTerm = flag.Bool("ignore-term", false, "ignore SIGTERM, so a graceful stop has to escalate to SIGKILL")
		chain      = flag.String("chain", "zpinit", "expected ancestors' comm, nearest first, ending at PID 1")
		envFile    = flag.String("envfile", "", "env file vmsetup writes; checked for presence and mode 0600")
		groupLead  = flag.Bool("group-leader", false, "expect to lead my own process group, so zpinit's stop signals and final SIGKILL can target the group")
		pid1Exe    = flag.String("pid1-exe", "/run/zpinit/bin/zpinit", "what /proc/1/exe must point at: the bundle's zpinit, never the image's")
		expectEnv  envList
	)
	flag.Var(&expectEnv, "env", "expected KEY=value, repeatable")
	flag.Parse()

	if *ignoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}

	c := &checker{}

	// Not a check, but the thing to look at first when one fails: whether
	// the root filesystem came up read-only, and what the host actually
	// passed on the command line.
	fmt.Printf("PROBE info cmdline                      %s\n", firstLine("/proc/cmdline"))

	mounts := readMounts()
	fmt.Printf("PROBE info root                         %s\n", mounts["/"].opts)
	// The root is the VM's own disk, writable, switched into from the
	// initramfs (SPEC.md §5).
	root, ok := mounts["/"]
	c.check("root is the writable disk", ok && root.fstype == "ext4" && strings.HasPrefix(root.opts, "rw"), describe(root, ok, "ext4"))
	// The bundle's zpinit and zpctl, on a read-only tmpfs of their own.
	bin, ok := mounts["/run/zpinit/bin"]
	c.check("zpinit bin tmpfs read-only", ok && bin.fstype == "tmpfs" && strings.HasPrefix(bin.opts, "ro"), describe(bin, ok, "tmpfs"))
	if fi, err := os.Stat("/run/zpinit/machine.json"); err == nil {
		c.check("machine.json mode", fi.Mode().Perm() == 0o600, fmt.Sprintf("mode=%04o size=%d", fi.Mode().Perm(), fi.Size()))
	} else {
		c.check("machine.json", false, err.Error())
	}
	// The initramfs was freed: its /init is gone from the old root, and
	// nothing of it is reachable from the new one.
	_, err := os.Stat("/init")
	c.check("no initramfs leftovers in /", os.IsNotExist(err), fmt.Sprintf("%v", err))

	// The standard table from SPEC.md §5. devtmpfs on /dev has to be
	// there for any of the rest to mean anything.
	for _, want := range []struct{ path, fstype string }{
		{"/dev", "devtmpfs"},
		{"/proc", "proc"},
		{"/sys", "sysfs"},
		{"/dev/pts", "devpts"},
		{"/dev/shm", "tmpfs"},
		{"/dev/mqueue", "mqueue"},
		{"/run", "tmpfs"},
		{"/sys/fs/cgroup", "cgroup2"},
	} {
		m, ok := mounts[want.path]
		c.check("mount "+want.path, ok && m.fstype == want.fstype, describe(m, ok, want.fstype))
	}

	// The hardening flags are the reason the table specifies them; a mount
	// that lost nosuid is a real regression, not a cosmetic one.
	for _, want := range []struct {
		path  string
		flags []string
	}{
		{"/dev", []string{"nosuid"}},
		{"/proc", []string{"nosuid", "nodev", "noexec"}},
		{"/sys", []string{"nosuid", "nodev", "noexec"}},
		{"/run", []string{"nosuid", "nodev"}},
		{"/sys/fs/cgroup", []string{"nosuid", "nodev", "noexec"}},
	} {
		m := mounts[want.path]
		missing := []string{}
		for _, f := range want.flags {
			if !strings.Contains(","+m.opts+",", ","+f+",") {
				missing = append(missing, f)
			}
		}
		c.check("flags "+want.path, len(missing) == 0, "opts="+m.opts+" missing="+strings.Join(missing, ","))
	}

	for _, link := range []string{"/dev/fd", "/dev/stdin", "/dev/stdout", "/dev/stderr"} {
		target, err := os.Readlink(link)
		c.check("symlink "+link, err == nil, fmt.Sprintf("-> %s %v", target, err))
	}

	if *hostname != "" {
		got, err := os.Hostname()
		c.check("hostname", err == nil && got == *hostname, fmt.Sprintf("%q (want %q)", got, *hostname))
		c.check("/etc/hostname", fileContains("/etc/hostname", *hostname), firstLine("/etc/hostname"))
		c.check("/etc/hosts self", fileContains("/etc/hosts", *hostname), "")
	}
	c.check("/etc/hosts localhost", fileContains("/etc/hosts", "127.0.0.1"), "")
	if *nameserver != "" {
		c.check("/etc/resolv.conf", fileContains("/etc/resolv.conf", "nameserver "+*nameserver), firstLine("/etc/resolv.conf"))
	}

	if *address != "" {
		c.check("address "+*iface, hasAddress(*iface, *address), addrList(*iface))
	}
	if *mtu != 0 {
		got := "no such interface"
		ok := false
		if link, err := net.InterfaceByName(*iface); err == nil {
			got = fmt.Sprint(link.MTU)
			ok = link.MTU == *mtu
		}
		c.check(fmt.Sprintf("mtu %s %d", *iface, *mtu), ok, got)
	}
	if *gateway != "" {
		c.check("default route", hasDefaultRoute(*gateway), *gateway)
	}
	c.check("loopback up", loopbackUp(), "")

	for _, want := range expectEnv {
		k, v, _ := strings.Cut(want, "=")
		got, present := os.LookupEnv(k)
		c.check("env "+k, present && got == v, fmt.Sprintf("%q (want %q)", got, v))
	}
	// HOME and TERM are what a container runtime would have supplied; SPEC.md §5
	// makes vmsetup supply them.
	for _, k := range []string{"HOME", "TERM", "PATH"} {
		v, present := os.LookupEnv(k)
		c.check("env "+k+" set", present && v != "", v)
	}

	// The env file is how the composed environment reaches entrypoint.d
	// scripts. 0600 because image env routinely carries credentials.
	if *envFile != "" {
		if fi, err := os.Stat(*envFile); err == nil {
			c.check("env_file mode", fi.Mode().Perm() == 0o600, fmt.Sprintf("%s mode=%04o size=%d", *envFile, fi.Mode().Perm(), fi.Size()))
		} else {
			c.check("env_file", false, err.Error())
		}
	}

	c.check("uid", os.Getuid() == *uid, strconv.Itoa(os.Getuid()))
	c.check("gid", os.Getgid() == *gid, strconv.Itoa(os.Getgid()))

	// vmsetup execs zpinit (SPEC.md §6), so PID 1 is zpinit and the
	// expected process tree is probe -> zpinit(1).
	got := ancestors()
	c.check("process chain", got == *chain, fmt.Sprintf("%q (want %q)", got, *chain))
	c.check("probe is not pid1", os.Getpid() != 1, strconv.Itoa(os.Getpid()))

	// Machine-mode detection for zpinit: "PID 1 in the initial PID namespace"
	// rests on that namespace's inode being the fixed PROC_PID_INIT_INO
	// (0xEFFFFFFC). The probe shares PID 1's namespace, so its own link is
	// the one zpinit would read. /proc/1/ns/pid needs ptrace-read access to
	// PID 1, so it is only compared when readable (root).
	const initPidNS = "pid:[4026531836]"
	self, err := os.Readlink("/proc/self/ns/pid")
	c.check("initial pid namespace", err == nil && self == initPidNS, fmt.Sprintf("%s %v", self, err))
	if one, err := os.Readlink("/proc/1/ns/pid"); err == nil {
		c.check("pid1 in initial pid namespace", one == initPidNS, one)
	}
	// Readable only with ptrace-read access to PID 1, so as root.
	if exe, err := os.Readlink("/proc/1/exe"); err == nil {
		c.check("pid1 is the bundle's zpinit", exe == *pid1Exe, exe)
	}
	if *groupLead {
		pgid := unix.Getpgrp()
		c.check("process-group leader", pgid == os.Getpid(), fmt.Sprintf("pgid=%d pid=%d", pgid, os.Getpid()))
	}

	if *serve != "" {
		peer, err := awaitPing(*serve, 30*time.Second)
		c.check("host reached guest", err == nil, fmt.Sprintf("from %s %v", peer, err))
	}

	fmt.Printf("PROBE-RESULT pass=%d fail=%d\n", c.pass, c.fail)

	if *hold {
		// Keeps the machine up until the harness requests a graceful
		// stop, which is what the stop scenarios exercise.
		for {
			time.Sleep(time.Hour)
		}
	}
	if c.fail > 0 {
		os.Exit(1)
	}
}

type mount struct {
	fstype string
	opts   string
}

// readMounts parses /proc/self/mounts into a map keyed by mountpoint. The last
// entry for a path wins, which is what the process actually sees.
func readMounts() map[string]mount {
	out := map[string]mount{}
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return out
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 4 {
			continue
		}
		out[unescape(fields[1])] = mount{fstype: fields[2], opts: fields[3]}
	}
	return out
}

// unescape decodes the octal escapes /proc/mounts uses for spaces and tabs.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func describe(m mount, ok bool, want string) string {
	if !ok {
		return "not mounted (want " + want + ")"
	}
	return m.fstype
}

func fileContains(path, want string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), want)
}

func firstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}

func hasAddress(name, cidr string) bool {
	link, err := net.InterfaceByName(name)
	if err != nil {
		return false
	}
	addrs, err := link.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if a.String() == cidr {
			return true
		}
	}
	return false
}

func addrList(name string) string {
	link, err := net.InterfaceByName(name)
	if err != nil {
		return err.Error()
	}
	addrs, _ := link.Addrs()
	var out []string
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return strings.Join(out, " ")
}

// hasDefaultRoute reads /proc/net/route rather than opening a netlink socket,
// to keep the probe independent of the library vmsetup itself uses. A default
// route is the entry with a zero destination and a zero mask.
func hasDefaultRoute(gateway string) bool {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return false
	}
	want := net.ParseIP(gateway).To4()
	if want == nil {
		return false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		// The gateway is little-endian hex in this file.
		n, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			continue
		}
		gw := net.IPv4(byte(n), byte(n>>8), byte(n>>16), byte(n>>24)).To4()
		if gw.Equal(want) {
			return true
		}
	}
	return false
}

func loopbackUp() bool {
	lo, err := net.InterfaceByName("lo")
	return err == nil && lo.Flags&net.FlagUp != 0
}

// ancestors walks the parent chain from the probe up to PID 1 and returns
// the comm of each ancestor, nearest first, comma-separated.
func ancestors() string {
	var out []string
	pid := os.Getppid()
	for range 8 {
		comm, ppid, err := stat(pid)
		if err != nil {
			return err.Error()
		}
		out = append(out, comm)
		if pid == 1 {
			break
		}
		pid = ppid
	}
	return strings.Join(out, ",")
}

// stat reads comm and ppid from /proc/<pid>/stat. comm is parenthesised and
// may contain spaces, so the fields after it are found from the last ')'.
func stat(pid int) (string, int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, err
	}
	s := string(b)
	open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || close < open {
		return "", 0, fmt.Errorf("unparseable /proc/%d/stat", pid)
	}
	f := strings.Fields(s[close+1:])
	if len(f) < 2 {
		return "", 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	ppid, err := strconv.Atoi(f[1])
	return s[open+1 : close], ppid, err
}

// awaitPing proves the network works end to end: the host fetches /ping from
// this listener, and the reply has to find its way back over the guest's
// address and routes. It is host-initiated because the boot-test host's
// firewall drops guest-initiated connections to it; in the routed /32 case
// the reply to the host's link-local address can only leave via the onlink
// default route, so that route is still what is being exercised.
//
// "PROBE serving" is the host's cue to start fetching.
func awaitPing(addr string, timeout time.Duration) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	defer ln.Close()
	got := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "pong")
		select {
		case got <- r.RemoteAddr:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	fmt.Printf("PROBE serving %s\n", addr)
	select {
	case peer := <-got:
		// Let the response flush before the server closes.
		time.Sleep(100 * time.Millisecond)
		return peer, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("no fetch from the host within %s", timeout)
	}
}
