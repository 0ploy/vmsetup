// Command vmprobe is the test payload for the boot matrix. It runs as an
// ordinary zpinit service inside the guest and checks, from the inside, that
// vminit actually did what PLAN.md §5 says it does.
//
// Everything it needs to know about the expected machine arrives on the
// command line, so test/build-rootfs.sh can generate the vminit config and
// this invocation from one set of values and the two cannot drift.
//
// Output goes to stdout, which zpinit forwards to the console and Firecracker
// forwards to the host's serial log file. One line per check, then a summary
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
	"strconv"
	"strings"
	"time"
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
	var (
		hostname   = flag.String("hostname", "", "hostname vminit was told to set")
		address    = flag.String("address", "", "interface address in CIDR form")
		iface      = flag.String("interface", "eth0", "interface name")
		gateway    = flag.String("gateway", "", "default gateway")
		nameserver = flag.String("nameserver", "", "expected nameserver in /etc/resolv.conf")
		uid        = flag.Int("uid", 0, "uid the service is expected to run as")
		gid        = flag.Int("gid", 0, "gid the service is expected to run as")
		probeURL   = flag.String("http", "", "URL on the host to fetch, proving the network works end to end")
		hold       = flag.Bool("hold", false, "sleep forever after reporting instead of exiting")
		expectEnv  envList
	)
	flag.Var(&expectEnv, "env", "expected KEY=value, repeatable")
	flag.Parse()

	c := &checker{}

	// Not a check, but the thing to look at first when one fails: whether
	// the root filesystem came up read-only, and what the host actually
	// passed on the command line.
	fmt.Printf("PROBE info cmdline                      %s\n", firstLine("/proc/cmdline"))

	mounts := readMounts()
	fmt.Printf("PROBE info root                         %s\n", mounts["/"].opts)
	// The standard table from PLAN.md §5.2. devtmpfs on /dev has to be
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
	if *gateway != "" {
		c.check("default route", hasDefaultRoute(*gateway), *gateway)
	}
	c.check("loopback up", loopbackUp(), "")

	for _, want := range expectEnv {
		k, v, _ := strings.Cut(want, "=")
		got, present := os.LookupEnv(k)
		c.check("env "+k, present && got == v, fmt.Sprintf("%q (want %q)", got, v))
	}
	// HOME and TERM are what a container runtime would have supplied; §5.5
	// makes vminit supply them.
	for _, k := range []string{"HOME", "TERM", "PATH"} {
		v, present := os.LookupEnv(k)
		c.check("env "+k+" set", present && v != "", v)
	}

	// The env file is how the composed environment reaches entrypoint.d
	// scripts. 0600 because image env routinely carries credentials.
	if fi, err := os.Stat("/run/zpinit/env"); err == nil {
		c.check("/run/zpinit/env", true, fmt.Sprintf("mode=%04o size=%d", fi.Mode().Perm(), fi.Size()))
		c.check("/run/zpinit/env mode", fi.Mode().Perm() == 0o600, fmt.Sprintf("%04o", fi.Mode().Perm()))
	} else {
		c.check("/run/zpinit/env", false, err.Error())
	}

	c.check("uid", os.Getuid() == *uid, strconv.Itoa(os.Getuid()))
	c.check("gid", os.Getgid() == *gid, strconv.Itoa(os.Getgid()))

	// vminit must have exec'd away: zpinit owns PID 1, and the probe is one
	// of its children rather than a child of a lingering vminit.
	c.check("pid1 is zpinit", strings.Contains(pid1Comm(), "zpinit"), pid1Comm())
	c.check("probe is not pid1", os.Getpid() != 1, strconv.Itoa(os.Getpid()))

	if *probeURL != "" {
		body, err := fetch(*probeURL)
		c.check("host reachable", err == nil, fmt.Sprintf("%s %v", body, err))
	}

	fmt.Printf("PROBE-RESULT pass=%d fail=%d\n", c.pass, c.fail)

	if *hold {
		// zpinit v0.5.5 has no VM mode yet, so a service that exits takes
		// zpinit with it and PID 1 exiting panics the kernel. Holding keeps
		// the machine alive for the host to inspect and shut down.
		// See ZPINIT_CHANGES.md §2.
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
// to keep the probe independent of the library vminit itself uses. A default
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

func pid1Comm() string {
	b, err := os.ReadFile("/proc/1/comm")
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(string(b))
}

// fetch proves the network works end to end: the address, the route and the
// host's TAP device all have to be right for this to return.
func fetch(url string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	// The host listener may not be up at the instant the guest boots.
	for range 20 {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		body := make([]byte, 64)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		return strings.TrimSpace(string(body[:n])), nil
	}
	return "", lastErr
}
