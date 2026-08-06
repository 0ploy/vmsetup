# Research notes

Findings behind the decisions in [../PLAN.md](../PLAN.md). Recorded so the
implementation session does not have to re-derive them.

Everything here was established on macOS in August 2026 against Firecracker
`main`, Docker 29.6.2, and the fly.io init snapshot. **Nothing involving a
running microVM could be tested** — no KVM on macOS. Claims that need
verification on real hardware are marked **[UNVERIFIED]**.

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

`ip(8)` is a thin wrapper over an `AF_NETLINK` socket. fly's init skips the
binary entirely — `fly-io-init/src/bin/init/main.rs:625-696` opens a netlink
connection and issues `RTM_SETLINK` / `RTM_NEWADDR` / `RTM_NEWROUTE` directly.
Because the init is a **static binary we ship**, the guest's distro is
irrelevant. That is the whole trick.

Go equivalent: `github.com/vishvananda/netlink`, pure Go, no cgo.

### Kernel `ip=` as a fallback

The guest kernel can configure one interface before init runs:

```
ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0-ip>:<dns1-ip>
```

Confirmed `CONFIG_IP_PNP=y`, `CONFIG_IP_PNP_DHCP=y`, `CONFIG_IP_PNP_BOOTP=y`
in **every** Firecracker reference guest config, both arches, 5.10 through
6.18 (`resources/guest_configs/microvm-kernel-ci-*.config`).

Limits: IPv4 only, effectively one interface, and it does **not** write
`/etc/resolv.conf` — DNS servers land in `/proc/net/pnp` instead. The classic
trick is symlinking `/etc/resolv.conf → /proc/net/pnp`, whose format is
resolver-compatible.

Useful as belt-and-braces, and essential for future systemd-passthrough mode.
Not sufficient as the primary mechanism.

---

## 2. What Docker's export actually contains

Tested against `nginx:alpine` and a `scratch` image built from busybox.

### Layer flattening works

`docker export` of a **created but never started** container flattens all
layers and resolves whiteouts. Verified: zero `.wh.` entries remained in the
`nginx:alpine` export. So layer-merge logic is not strictly required *if* you
go through the daemon — but see §3 for why you should not.

### Docker injects synthetic files, even into `scratch`

Full contents of the `scratch` image export:

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
a VM nothing mounts over them, so **DNS silently fails unless vminit writes
them**. `/dev/console` being a regular file is shadowed once devtmpfs is
mounted over `/dev`, but should be cleaned anyway.

### Symlink density

Alpine is roughly 400 busybox symlinks (`bin/cat -> /bin/busybox`); Debian is
usrmerge (`/lib -> /usr/lib`). Extraction must preserve symlinks and hardlinks
faithfully and must not follow them.

---

## 3. Do not build on the Docker daemon

On Docker 29.6.2 with the containerd image store, after
`docker pull --platform linux/amd64 nginx:alpine` on an arm64 host:

```
$ docker image inspect nginx:alpine
  → "Config": {}          ← silently empty, no error, exit 0

$ docker image inspect --platform linux/amd64 nginx:alpine
  → full config

$ docker save nginx:alpine -o img.tar
  → Error: unable to create manifests file: NotFound: content digest
    sha256:1dd3048a... not found
```

An empty config with a zero exit code is the worst possible failure mode: the
resulting VM would boot with no entrypoint, no env, and no user, and the
packer would report success.

Use `skopeo` or `crane` against the registry, resolve the index to the
platform manifest explicitly, and read the config blob. Also removes the
daemon as a build dependency.

---

## 4. Console, serial, and boot time **[partially UNVERIFIED]**

Firecracker has **no virtio-console**. The legacy 8250 UART is the only console
device it emulates — confirmed by the device tree under
`src/vmm/src/devices/`: `legacy/` contains `i8042.rs`, `rtc_pl031.rs`,
`serial.rs`, and nothing else console-shaped.

Every byte written to that UART is a synchronous VM exit. The kernel boot log
is hundreds of lines, so `console=ttyS0` measurably slows cold boot.

### The device exists even without `console=`, on x86_64

`src/vmm/src/device_manager/mod.rs:201-235` — `create_legacy_devices` calls
`setup_serial_device` **unconditionally** on x86_64, and is itself called
unconditionally from `DeviceManager::new` (line 241). There is no `console=`
check on this path.

The `cmdline_contains_console` check at line 364 is in the **aarch64** branch,
where the serial is an MMIO device that must be advertised on the cmdline.

Reference guest configs set `CONFIG_SERIAL_8250_RUNTIME_UARTS=1`, so the driver
still enumerates `/dev/ttyS0`.

**Conclusion:** on x86_64 you can drop `console=` for a fast boot and still
have userspace write to `/dev/ttyS0`.

> **[UNVERIFIED]** The swagger description for `PUT /serial` states it "has no
> effect if the serial console is not also enabled on the guest kernel command
> line," which contradicts the above. The doc is most likely written for the
> aarch64 case. **Test on real hardware before depending on this.**

### Serial output destinations

`SerialOutInner` (`devices/legacy/serial.rs:130-140`) supports `Stdout`,
`File`, and `Sink`. `PUT /serial` with `serial_out_path` sends guest serial
output straight to a **per-VM file on the host** — no vsock required for logs.

There is also a `TokenBucket` rate limiter on serial output, which matters for
untrusted workloads: a chatty guest is throttled, not able to flood the host.

### Boot arg notes

`i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd` saves tens of ms of
keyboard probing — recommended by Firecracker's own `docs/api_requests/actions.md`.
Keep the i8042 **driver** enabled though; `SendCtrlAltDel` needs
`CONFIG_SERIO_I8042` and `CONFIG_KEYBOARD_ATKBD`.

Prefer uncompressed `vmlinux` over `bzImage`: decompression costs boot time and
guest memory.

---

## 5. Shutdown

Firecracker **exits the VMM process on CPU reset**. So a guest terminates its
own VM by calling `reboot(RB_AUTOBOOT)` — this is what fly does
(`fly-io-init/src/bin/init/main.rs:879-881`).

Conversely, if PID 1 merely exits, the kernel panics with
`Attempted to kill init!` and the VM hangs until killed externally.

Host-initiated graceful shutdown on x86 is the `SendCtrlAltDel` action, which
emulates an i8042 keyboard event. By default the **kernel** handles that with
an immediate hard reset. PID 1 must call `reboot(LINUX_REBOOT_CMD_CAD_OFF)` to
opt out, after which the kernel delivers **SIGINT to PID 1** instead.

There is no ACPI power button. On aarch64 there is no i8042 at all, so a
future aarch64 port needs a different trigger.

---

## 6. MMDS

### One network interface is enough

Firecracker's MMDS user guide attaches MMDS to `eth0` in its own example:

```sh
MMDS_NET_IF=eth0
ip route add 169.254.169.254 dev ${MMDS_NET_IF}
```

Firecracker intercepts packets addressed to the MMDS IP on the configured
interface and answers them itself; other traffic passes through to the TAP. A
dedicated MMDS interface is unnecessary — fcm's current second TAP (`mmds0`)
can be removed.

### Mutability

MMDS is a store inside the Firecracker process. `PUT /mmds` replaces,
`PATCH /mmds` merges (RFC 7396 JSON Merge Patch) — **at any time, including
while the VM is running**. That is its unique capability.

What is fixed pre-boot is the *configuration*: which NICs may reach it, the IP,
and the version. You cannot add MMDS to a VM that booted without it.

There is no push notification. The guest must poll.

### The bootstrapping problem

MMDS is reached *over the network*, but the thing most needed at boot is the
network configuration. Hence the split in PLAN.md §4.1: boot-critical config
comes from a file baked into the rootfs; MMDS handles only what must change on
a live VM.

### V1 vs V2

V1 is deprecated and unauthenticated — any guest process can read it, including
via SSRF through the tenant's own application. V2 requires a token obtained
with `PUT /latest/api/token`. **Use V2.** Note fcm currently omits the
`version` field, which defaults to V1.

---

## 7. Why the split between vminit and zpinit

`tini` (`tini/src/tini.c`, 688 lines) does exactly two things: `waitpid`
reaping and signal forwarding, plus `PR_SET_CHILD_SUBREAPER`. It does none of
the machine bringup, so it does not reduce the work — and zpinit already
supersedes it.

zpinit covers the other half well: centralized `wait4(-1, WNOHANG)` reaping
with PID dispatch, `entrypoint.d`, a supervision state machine with backoff and
readiness probes, replicas, and a control socket.

One useful accident: `internal/resources` takes the **minimum of cgroup limits
and `/proc`**. A microVM has no cgroup limits — the VM's size *is* the limit —
so detection falls through to `/proc` and reports the VM's actual vCPU count
and RAM. `ZPINIT_CPU_COUNT` and `ZPINIT_MEMORY_BYTES` come out correct with no
changes. Untested in that configuration; worth a test.

What zpinit lacks for VM use is written up in `ZPINIT_CHANGES.md`. The critical
items are: reboot instead of exit as PID 1, do not `syscall.Exec` in VM mode,
and handle Ctrl+Alt+Del.

### Why handoff is `execve`, not fork

zpinit does **not** set `PR_SET_CHILD_SUBREAPER` — verified by grep across
`internal/` and `cmd/`. Its reaper package documents the assumption directly:
"As PID 1 zpinit must reap every child, including orphans reparented to us from
anywhere in the container after a double-fork."

If vminit stayed PID 1 and forked zpinit as PID 2, there would be **no reaping
race** — `wait4(-1)` only reaps a process's own children, and the two
parent/child sets are disjoint. But orphans from double-forks would reparent to
vminit rather than zpinit. Nothing breaks functionally, since zpinit has no
channel registered for untracked orphans anyway. The real cost is that code
written against a documented "I am PID 1" invariant would run somewhere that
invariant is false — a latent trap rather than a present bug.

The decisive argument is elsewhere: **systemd must be PID 1.** It checks
`getpid() == 1` and refuses to run as a system manager otherwise. So `exec` is
mandatory for distro images regardless. Given it is also correct for zpinit,
one handoff mechanism covering both beats two mechanisms with two shutdown
paths and two reaping models.

`supervise` mode survives only for the bare-command case, where it solves a
genuine problem: without it, an unsupervised entrypoint becomes PID 1 and its
exit panics the kernel unless the workload itself calls `reboot()`.

### systemd images: generate config, do not apply it

systemd owns mounts, cgroups and network management. It has mount units for
`/proc`, `/sys`, `/dev`, `/run` and `/sys/fs/cgroup` and expects to own them.
systemd-networkd with a wildcard match will reconfigure a netlink-configured
interface. On systemd-resolved systems `/etc/resolv.conf` is a symlink to a
stub, so direct writes get overwritten.

So for systemd targets vminit's job inverts: write configuration files the
target will read, then exec — never configure imperatively.

Better still for the common case: **MMDS is already EC2-shaped.** That is why
Firecracker's own documentation examples use `ami-id`, `reservation-id` and
`local-hostname` under `latest/meta-data`. cloud-init ships in every cloud
image and its Ec2 datasource consumes that layout directly, so an Ubuntu cloud
image needs no vminit at all — boot `/sbin/init`, populate MMDS in EC2 format.
Established pattern; see the OnGres writeup and `firebuild` in Sources.

Caveat to set expectations with: a systemd VM boots in seconds rather than
milliseconds, wants a writable rootfs, and realistically needs 256 MB or more.
Firecracker's fast-boot property does not survive a full distro.

### Why Go is fine for vminit despite the runtime

vminit `execve`s its handoff target, which **replaces the entire process
image**. The Go runtime, its threads and its heap are gone. Steady-state
footprint is exactly zero; the runtime only exists for the tens of milliseconds
vminit is alive. Boot-time contribution is single-digit ms in any language —
the real costs are kernel decompression, device probing, and console output.

Requires `CGO_ENABLED=0`, which also gives the pure-Go `os/user` that parses
`/etc/passwd` directly rather than going through NSS. That is the desired
behavior for a container image.

---

## 8. Prior art

- **fly.io init** (`fly-io-init/`) — the closest reference. Two-drive layout
  (`/dev/vda` init + config, `/dev/vdb` rootfs) with a hand-rolled switch_root
  via `chdir` + `MS_MOVE` + `chroot` (`main.rs:214-232`). Config injected as
  `/fly/run.json`. vsock server on port 10000 providing exec with PTY
  (`src/api/exec.rs`, `src/pty/mod.rs`) — their `docker exec`. Reads
  `/dev/kmsg` after a child dies to detect OOM kills (`main.rs:801-833`), a
  trick worth copying since a VM has no cgroup OOM event to observe.
- **Weave Ignite** (archived) — did exactly this task: pulled an OCI image,
  created a container, exported the rootfs into an ext4 file, booted it as a
  Firecracker VM in ~125ms. Closest existing prior art end to end.
- **firecracker-containerd** — devmapper thin-provisioning snapshotter exposing
  container images as block devices. Relevant if cross-VM image dedup becomes a
  requirement.

---

## Sources

- [Firecracker: rootfs and kernel setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/rootfs-and-kernel-setup.md)
- [Firecracker: network setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md)
- [Firecracker: actions API](https://github.com/firecracker-microvm/firecracker/blob/main/docs/api_requests/actions.md)
- [Firecracker: MMDS user guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/mmds/mmds-user-guide.md)
- [Firecracker: vsock](https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md)
- [Firecracker: snapshot support](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md)
- [Firecracker: production host setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)
- [Linux kernel: nfsroot / `ip=` and `/proc/net/pnp`](https://docs.kernel.org/admin-guide/nfs/nfsroot.html)
- [Weave Ignite](https://github.com/weaveworks/ignite)
- [firecracker-containerd snapshotter](https://github.com/firecracker-microvm/firecracker-containerd/blob/main/docs/snapshotter.md)
- [OnGres: running vanilla cloud images on Firecracker](https://ongres.com/blog/automation-to-run-vms-based-on-vanilla-cloud-images-on-firecracker/) — cloud-init + MMDS
- [firebuild](https://gruchalski.com/posts/2021-03-23-introducing-firebuild/)
