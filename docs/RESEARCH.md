# Research notes

Findings behind the decisions in [../PLAN.md](../PLAN.md), recorded so nobody
has to re-derive them. **[VERIFIED]** means tested on real hardware: Linux
x86_64 (AMD), Firecracker v1.16.1–v1.17.0, guest kernel 6.18.39.

Host-side findings (the VMM, the kernel command line, guest kernel builds,
serial configuration, boot-time measurement) belong to
[fctl](https://github.com/0ploy/fctl). These are the guest-side ones.

---

## 1. Why the network must be configured from inside

In a container, the host creates a veth pair and moves one end into the
container's netns, configuring it *from the host*. The container is passive.

A microVM has no shared namespace. The host creates a **TAP** device and hands
the fd to the Firecracker process, which exposes it to the guest as a
**virtio-net** device. There is no path for the host to configure the guest's
`eth0`. It must happen guest-side.

### The distro-independence problem

The obvious commands are iproute2:

```sh
ip addr add 172.16.0.2/30 dev eth0
ip link set eth0 up
ip route add default via 172.16.0.1 dev eth0
```

`scratch` has no iproute2. Alpine has busybox `ip`, which differs from
Debian's. Depending on any of this makes the tool image-dependent.

`ip(8)` is a thin wrapper over an `AF_NETLINK` socket. fly.io's init
([init-snapshot](https://github.com/superfly/init-snapshot)) skips the binary
entirely: it opens a netlink connection and issues `RTM_SETLINK`,
`RTM_NEWADDR` and `RTM_NEWROUTE` directly. Because the init is a **static
binary we ship**, the guest's distro is irrelevant. That is the whole trick.

Go equivalent: `github.com/vishvananda/netlink`, pure Go, no cgo.

### Kernel `ip=` — rejected

The guest kernel can configure one interface before init runs:

```
ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0-ip>:<dns1-ip>
```

`CONFIG_IP_PNP` is set in every Firecracker reference guest config. But it is
IPv4 only, effectively one interface, and does **not** write
`/etc/resolv.conf` (DNS servers land in `/proc/net/pnp`). And it cannot express
the routed `/32` every VM gets: `ipconfig` initialises its netmask to
`INADDR_NONE`, so a `/32` is indistinguishable from "unset". vmsetup
configures the interface over netlink, and a VM whose network bringup failed
is reached over exec, not a debug NIC (PLAN.md §7.2).

---

## 2. What Docker's export contains

Tested against `nginx:alpine` and a `scratch` image built from busybox. Docker
injects synthetic files, even into `scratch`:

```
-rwxr-xr-x  0  .dockerenv
-rwxr-xr-x  0  busybox                 (the only real file)
drwxr-xr-x  0  dev/
-rwxr-xr-x  0  dev/console             ← 0-byte REGULAR FILE, not a device node
drwxr-xr-x  0  dev/pts/
drwxr-xr-x  0  dev/shm/
drwxr-xr-x  0  etc/
-rwxr-xr-x  0  etc/hostname            ← 0 bytes
-rwxr-xr-x  0  etc/hosts               ← 0 bytes
lrwxrwxrwx  0  etc/mtab -> /proc/mounts
-rwxr-xr-x  0  etc/resolv.conf         ← 0 bytes
drwxr-xr-x  0  proc/
drwxr-xr-x  0  sys/
```

The three `/etc` files are empty because Docker bind-mounts them at runtime. In
a VM nothing mounts over them, so **DNS silently fails unless vmsetup writes
them**. The fake `/dev/console` is shadowed once devtmpfs is mounted over
`/dev`. An image built straight from OCI layers has none of these, and vmsetup
handles both.

---

## 3. Console and logging

On x86_64 Firecracker creates COM1 unconditionally, so `/dev/ttyS0` is present
and writable from guest userspace whether or not `console=` is on the cmdline
[VERIFIED]. With `console=` absent no getty claims the port, so the guest has it
to itself. vmsetup (for its log and park line) and zpinit (for the reset line)
therefore open `/dev/ttyS0` directly and never assume a console device.

The port is rate-limited by the host (Firecracker applies a token bucket), so
writes can be silently dropped: serial is not a reliable channel, which is why
nothing the host acts on strongly travels over it.

Upstream's own performance boot args include `8250.nr_uarts=0`, which removes
the port entirely. If that is ever adopted, logging and the park and reset
lines have to move to vsock.

---

## 4. Shutdown

Firecracker **exits the VMM process on CPU reset**. So a guest ends its own VM
by calling `reboot(RB_AUTOBOOT)` with `reboot=k` on the cmdline, as fly.io's
init does.

If PID 1 merely exits, the kernel panics with `Attempted to kill init!`.
**PID 1 must never return**; its terminal action is always a `reboot()`. That
is why zpinit's machine mode turns every exit path into sync and reboot, and
why vmsetup, while it is PID 1, catches every signal and reboots when it has
no zpinit to hand over to.

`SendCtrlAltDel` arrives as an i8042 keyboard event. By default the **kernel**
handles it with an immediate hard reset, which gives the workload no chance to
flush. PID 1 calls `reboot(LINUX_REBOOT_CMD_CAD_OFF)` to opt out, after which
the kernel delivers **SIGINT to PID 1** instead [VERIFIED]. In the VM that is
zpinit's machine mode. A Ctrl+Alt+Del sent before the guest's i8042/atkbd
driver has finished probing is lost [VERIFIED], so the host resends
(PLAN.md §5.7).

There is no ACPI power button. On aarch64 there is no i8042 at all, so an
aarch64 port needs a different trigger.

---

## 5. MMDS

An in-guest MMDS agent is deferred (PLAN.md §12). What it would have to deal
with, if built:

- **Mutability is the whole point.** `PUT /mmds` replaces and `PATCH /mmds`
  merges (RFC 7396), at any time, including while the VM runs. It is the only
  channel that can reconfigure a live VM without a reboot.
- **Bootstrapping.** MMDS is reached *over the network*, but the thing most
  needed at boot is the network configuration. So boot-critical config arrives
  on the config blob (PLAN.md §4.1), and an agent must never be on the
  critical path to a working network.
- **No push notification.** The guest polls, so any poll interval is a latency
  floor on every live reconfiguration. Push over exec does the job better,
  which is why the agent is deferred.
- **V2 only.** V1 is unauthenticated: any guest process can read it, including
  via SSRF through the tenant's own application. V2 needs a token from
  `PUT /latest/api/token`, fetched, cached and refreshed. An agent must refuse
  to fall back to V1 even if the host configured it.

---

## 6. Why the split between vmsetup and zpinit

**Bringup** is what a container runtime does: runc prepares the machine once
and is finished. **Being PID 1** is process management: reaping, signals,
stopping, deciding what the workload's exit means. That is tini's job, and
zpinit's. The split follows the job:

- **vmsetup** is bringup only and `execve`s zpinit. It manages no process at
  any point. Both binaries ship in an initramfs the host attaches on every
  boot, not in the image, which keeps a workload that owns its writable disk
  from owning PID 1 on the next boot.
- **zpinit** is PID 1 for the life of the VM, so exactly one process manager
  runs per VM. The duties that only exist for the kernel's init (CAD_OFF, sync
  and reboot instead of exit, the reset line, the vsock exec listener, rescue)
  are an additive **machine mode**, specified in
  [zpinit#10](https://github.com/0ploy/zpinit/issues/10). The duties that are
  just good PID 1 behaviour (surviving every signal, a tini-style wrap mode
  that stays resident above the command) are ordinary zpinit features that
  containers benefit from too.

What zpinit already brings: centralized `wait4(-1, WNOHANG)` reaping with PID
dispatch, `entrypoint.d`, a supervision state machine with backoff and
readiness probes, replicas, and a control socket. One useful accident: its
resource detection takes the **minimum of cgroup limits and `/proc`**. A
microVM has no cgroup limits, since the VM's size *is* the limit, so detection
falls through to `/proc` and reports the VM's actual vCPU count and RAM.
Untested in that configuration (zpinit#10).

Machine mode is **detected, not configured** (below), so no flag exists that a
container user could set by mistake. The exec listener additionally needs
`/run/zpinit/machine.json`, which vmsetup writes and zpinit ignores outside
machine mode. Up to and including 6.18, vsock is not network-namespaced, and
the af_vsock namespace modes added in later kernels default to `global`, so a
container that opened a listener could be reached by the hypervisor of the
host it runs on (checked against `net/vmw_vsock/af_vsock.c`).

The kernel facts behind the container-mode guard, checked against v6.18
`kernel/reboot.c` and `kernel/pid_namespace.c`: inside a child PID namespace,
`reboot()` first needs `CAP_SYS_BOOT` in that namespace's user namespace (EPERM
under Docker's default capabilities). If it gets past that,
`reboot_pid_ns()` turns `RESTART` into SIGKILL of the namespace's init, with
the container exiting as *killed by SIGHUP*, and rejects `CAD_OFF` with
`EINVAL`. Calling either in a container is never harmless, which is why both
sit behind the detection.

### Telling "the kernel's init" from "a container's init" [VERIFIED]

A process can tell, without a flag, whether it is PID 1 of the machine or PID 1
of a container: `getpid() == 1` **and** `readlink /proc/self/ns/pid` is
`pid:[4026531836]`. That number is `PROC_PID_INIT_INO` (`0xEFFFFFFC`), the
fixed inode of the kernel's initial PID namespace; every other PID namespace
gets a dynamically allocated inode. Checked on the host (`unshare --pid` gives
`pid:[4026532817]`, PID 1, no `/proc/2`) and in the guest by the boot probe:
both `/proc/self/ns/pid` and `/proc/1/ns/pid` read `pid:[4026531836]`.
`/proc/1/ns/pid` is only readable with ptrace-read access to PID 1, so an
unprivileged process reads its own link instead.

It detects "the kernel's init", not "a VM": bare metal booted with
`init=zpinit` also matches, and wants the same behaviour. Kata and
firecracker-containerd run the workload in a PID namespace inside the guest,
so they correctly read as container. It guards against accidents, not
deliberate fakes; a container can only fool its own `/proc`.

### Rejected, with the reason

| Option | Why not |
| --- | --- |
| The bringup binary stays PID 1 and forks zpinit (or the workload) | Two process managers per VM: two reapers, two stop sequences, and two stop timeouts that have to nest. Bringup is not process management |
| A fallback lifecycle in vmsetup for when zpinit cannot be exec'd | The same mistake, kept for a host-side bug. A broken bundle parks and reboots instead (PLAN.md §6.2) |
| `execve` the bare workload as PID 1, no zpinit | PID 1 gets no default signal dispositions (a workload without a `SIGTERM` handler ignores the stop), nothing reaps orphans, nothing reports the exit status, and the workload's exit panics the kernel |
| Handoff "profiles" for distro images (`cloud-init`, `systemd`, `openrc`) with generated network config | Distro images boot without the init bundle. MMDS is already EC2-shaped, so cloud-init's Ec2 datasource configures an Ubuntu cloud image with no guest init of ours |
| A `--vm` flag on zpinit | A flag can be set by mistake in a container. Detection cannot |
| Guest binaries baked into the image | The workload owns its writable disk and would own the next boot |
| `pivot_root` for the switch | It refuses the initramfs, which is the rootfs mount itself; `MS_MOVE` + `chroot` is the standard `switch_root` (PLAN.md §5.2) |

### Why Go is fine for vmsetup despite the runtime

vmsetup `execve`s zpinit, which **replaces the entire process image**. The Go
runtime, its threads and its heap are gone; the runtime only exists for the
tens of milliseconds vmsetup is alive. The real boot costs are kernel
decompression, device probing and console output, not the language.

Requires `CGO_ENABLED=0`, which also gives the pure-Go `os/user` that parses
`/etc/passwd` directly rather than going through NSS: the desired behaviour
for a container image.

---

## 7. Prior art

- **fly.io init** ([init-snapshot](https://github.com/superfly/init-snapshot))
  is the closest reference: a hand-rolled switch_root via `chdir` + `MS_MOVE` +
  `chroot`, netlink network setup, and a vsock exec server with PTY. It reads
  `/dev/kmsg` after a child dies to detect OOM kills, a trick worth copying,
  since a VM has no cgroup OOM event to observe.
- **Weave Ignite** (archived) did this task end to end: pulled an OCI image,
  exported the rootfs into an ext4 file, and booted it as a Firecracker VM in
  ~125 ms.
- **firecracker-containerd**: a devmapper snapshotter exposing container images
  as block devices, and an in-guest agent that runs the workload in a PID
  namespace.

---

## Sources

- [Firecracker: rootfs and kernel setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/rootfs-and-kernel-setup.md)
- [Firecracker: network setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md)
- [Firecracker: actions API](https://github.com/firecracker-microvm/firecracker/blob/main/docs/api_requests/actions.md)
- [Firecracker: MMDS user guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/mmds/mmds-user-guide.md)
- [Firecracker: vsock](https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md)
- [Firecracker: snapshot support](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md)
- [Linux kernel: ramfs, rootfs and initramfs](https://docs.kernel.org/filesystems/ramfs-rootfs-initramfs.html)
- [Linux kernel: nfsroot / `ip=` and `/proc/net/pnp`](https://docs.kernel.org/admin-guide/nfs/nfsroot.html)
- [Weave Ignite](https://github.com/weaveworks/ignite)
- [firecracker-containerd snapshotter](https://github.com/firecracker-microvm/firecracker-containerd/blob/main/docs/snapshotter.md)
- [OnGres: running vanilla cloud images on Firecracker](https://ongres.com/blog/automation-to-run-vms-based-on-vanilla-cloud-images-on-firecracker/) — cloud-init + MMDS
