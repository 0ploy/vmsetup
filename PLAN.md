# vmsetup — design and spec

This document is the spec. Background and evidence for the decisions live in
[docs/RESEARCH.md](docs/RESEARCH.md); read that first if a choice here looks
arbitrary.

**In brief.** vmsetup only makes the VM bootable, then **always `execve`s
[zpinit](https://github.com/0ploy/zpinit)**, which is PID 1 for the life of the
VM. vmsetup does no process management at any point: no fork, no reaping, no
stop handling. It ends the VM only when there is no zpinit to hand it to
(§6.2). Everything after the `execve` is zpinit's **machine mode**
([zpinit#10](https://github.com/0ploy/zpinit/issues/10)), which zpinit switches
on by detecting that it is the kernel's own init.

vmsetup, zpinit and zpctl reach the guest in the **init bundle**, an initramfs
the host attaches on every boot, so the image carries no guest binaries. The
host side of every interface here is [fctl](https://github.com/0ploy/fctl), the
VM manager.

---

## 1. Scope

**In scope**

- `vmsetup` is `/init` of the init bundle. It reads the config, mounts the
  VM's disk and switches root into it, brings the machine up, writes zpinit's
  machine settings and `execve`s zpinit.
- **The init bundle**: the uncompressed cpio carrying vmsetup, zpinit and zpctl
  (§8.2), built reproducibly by `cmd/mkbundle` and attached by fctl.
- **The guest config schema** (§4): vmsetup reads it, fctl writes it.

**Out of scope**

- Everything once zpinit is running: reaping, the graceful stop, ending the VM,
  the reset line, `exec` over vsock and rescue. That is zpinit's machine mode;
  this document states only what vmsetup hands it (§5.6) and the parts fctl
  matches (§5.7).
- Everything on the host: VM lifecycle, TAP devices, the kernel command line,
  guest kernel builds, building images from OCI, attaching the bundle,
  composing the per-launch config. That is fctl.
- Full-distro images (systemd as PID 1). They boot without the init bundle.
- Live migration and aarch64 (§12).

**Target platform:** x86_64, Linux guest kernel 5.10+.

---

## 2. Design principles

1. **vmsetup is bringup, not process management.** It is the part of a
   container runtime that prepares the machine: runc's job, not tini's. It
   runs once, as PID 1 only because the kernel starts it there, and
   `execve`s zpinit, which inherits PID 1. This leaves exactly one process
   manager in the VM.
2. **zpinit is always the handoff target.** Plain Docker images run under
   zpinit's tini-style wrap mode, zpinit-native images under its supervise mode
   (§4.3). zpinit stays free of OCI and hypervisor knowledge: image-derived
   settings reach it through argv, `envp` and its own config, VM-specific ones
   through one file vmsetup writes (§5.6), which zpinit reads only in machine
   mode.
3. **No dependency on the guest image.** Everything is done via direct
   syscalls from a static binary the host supplies. vmsetup never shells out
   to `ip`, `mount`, `hostname` or anything else: `scratch` images have no
   userspace at all.
4. **Enforce on the host, never in the guest.** The workload is untrusted. All
   limits (vCPU, memory, I/O, network) are enforced by Firecracker and the
   host. vmsetup enforces nothing.
5. **Fail loudly, never silently.** A misconfigured network that boots anyway
   is worse than a VM that refuses to start with a clear message. Refusing
   means **parking** (§6.2), not crashing: the failure is reported on the
   serial log, the machine stays up and inspectable under zpinit's rescue
   mode, and no workload starts.
6. **Security and isolation come first.** Performance is taken only where it is
   harmless to isolation. The workload is untrusted and the VM boundary is the
   only boundary. The guest-side consequences are in §9.
7. **No merging in the guest.** The config's writer is fctl and its reader is
   vmsetup, and drift between them is the likeliest bug in the design. So every
   field arrives already resolved, unknown fields are rejected, and vmsetup
   applies defaults only where it alone knows the answer (`HOME` from the
   resolved user, §5.5).

---

## 3. Boot sequence

```
Firecracker   initrd_path=init.cpio, no root device
  └─ kernel   vmsetup_config=/dev/vda reboot=k panic=1 …   (no init=, no root=)
       └─ /init = vmsetup, in the initramfs   ← PID 1, for ~20–35 ms
            1. mount devtmpfs on /dev; stdio → /dev/null + console; catch every signal
            2. mount /proc; read vmsetup_config= from /proc/cmdline
            3. read and validate the config blob                   §4.1
            ── anything below that fails → park (§6.2) ──
            4. mount root.device on /newroot; switch root into it   §5.2
            5. mount filesystems                                    §5.2
            6. configure network via netlink                        §5.3
            7. hostname, /etc/hosts, /etc/resolv.conf               §5.4
            8. resolve users, compose env, write /run/zpinit/env    §5.5
            9. write /run/zpinit/machine.json                       §5.6
           10. execve zpinit (the bundle's copy)                    §5.6
       └─ zpinit                              ← PID 1, for the life of the VM (machine mode)
```

There is no root device and no `init=`. The kernel unpacks the init bundle
into a ramfs and runs its `/init`. If that cannot be executed, the kernel falls
back to mounting `root=`, finds none, and panics; with `panic=1` that resets.
So the boot **fails closed**: nothing on the workload's disk is ever booted by
accident.

vmsetup deliberately does not call `reboot(CAD_OFF)`; that is zpinit's first
act. A Ctrl+Alt+Del during vmsetup's few milliseconds resets the VM on the
spot, which is an acceptable answer to a stop request that early, and a
`SIGINT` caught then would be lost across the `execve` anyway.

---

## 4. Configuration

### 4.1 One document, raw bytes on a block device

The guest config is **one JSON document**, composed in full by fctl for each
launch and delivered as raw bytes on a dedicated read-only block device. There
is no filesystem on that device and no config file in the image.

```
offset  0   "FCTLCFG\0"        8 bytes, magic
offset  8   version            u32 little-endian; this document describes 1
offset 12   length             u32 little-endian, bytes of JSON that follow
offset 16   JSON document      NUL-padded to the end of the device
```

The device is 64 KiB. fctl validates the composed document against that ceiling
host-side, so the guest never meets a truncated document.

**Finding the device.** The kernel command line names it:
`vmsetup_config=/dev/vda`. vmsetup reads `/proc/cmdline`. There is deliberately
no dot in the parameter name, because dotted parameters are module parameters
and may never reach init; the undotted form survives into `/proc/cmdline`
unchanged [VERIFIED — guest kernel 6.18.39, Firecracker v1.17.0]. vmsetup opens
exactly the device named and **fails loudly** on a missing parameter, a wrong
magic, an unknown version, a `length` that runs past the device, or JSON that
does not decode. It never searches for the config elsewhere: a drive-ordering
bug must produce "this is not the config device", not a parse of somebody's
data volume.

fctl attaches the config first and flags no root device, so Firecracker names
drives in attach order: the config is `/dev/vda`, the root disk `/dev/vdb`,
data volumes `/dev/vdc` onwards. vmsetup relies on none of that. Every device
except the config device is *named in the document*, and the config device is
named on the cmdline.

Unknown fields in the document are an error, not ignored: silently ignoring a
field fctl thinks it set is exactly the drift principle 7 is about. The header
version is bumped for any change vmsetup cannot read compatibly.

### 4.2 Schema

```jsonc
{
  "hostname": "web-01",

  // What zpinit runs once vmsetup execs it. Fully resolved by fctl (§8.1).
  "handoff": {
    // The Docker command: entrypoint + cmd. Present → zpinit wrap mode runs it
    // as its one child. Absent → zpinit supervise mode runs the services in
    // the image's /etc/zpinit, and user/working_dir must be absent too.
    "command":      ["/docker-entrypoint.sh", "nginx", "-g", "daemon off;"],
    "user":         "nginx",          // user[:group]; names or numeric ids. With command only
    "working_dir":  "/",              // with command only
    "stop_signal":  "SIGQUIT",        // what zpinit sends the command on graceful stop
    "stop_timeout": "10s"             // zpinit's whole stop budget; below fctl's own stop timeout
  },

  // The composed environment, final. OCI form: KEY=value, order significant.
  "env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
          "NGINX_VERSION=1.31.3", "DEPLOY_ENV=prod"],

  "network": {
    "interfaces": [
      {
        "name":    "eth0",
        "mac":     "06:00:AC:10:00:02",   // optional; pin by MAC when set
        "address": "10.77.0.5/32",
        "gateway": "169.254.0.1",
        "mmds":    false                  // add a host route to the MMDS IP
      }
    ],
    "nameservers":  ["1.1.1.1", "8.8.8.8"],
    "search":       [],
    "extra_hosts":  [{ "ip": "10.0.0.5", "host": "db.internal" }]
  },

  // The VM's disk. vmsetup mounts it and switches root into it (§5.2).
  "root": { "device": "/dev/vdb", "fstype": "ext4", "options": "discard" },

  // Extra data volumes, mounted into the new root after §5.2's table.
  "mounts": [
    { "device": "/dev/vdc", "path": "/data", "fstype": "ext4", "options": "relatime" }
  ],

  // Optional. Absent → zpinit opens no exec listener. vmsetup only validates
  // this and passes it on (§5.6).
  "exec": {
    "port":        1024,    // vsock port zpinit listens on
    "user":        "nginx", // default identity for an exec request: the image's OCI User
    "working_dir": "/"
  },

  "logging": {
    "target": "serial"      // "serial" | "none"
  }
}
```

**Validation, before anything acts on the document.** Nothing is defaulted
(principle 7), so every field anything acts on is required:

- `root.device` and `root.fstype`; `root.options` must not contain `ro`.
- In `handoff`: `stop_signal` and `stop_timeout` always; `user` and
  `working_dir` exactly when `command` is present; `command`, when present, is
  non-empty.
- `logging.target`, and all three fields of `exec` when it is present.
- `root.device`, the `working_dir`s and every `mounts[].path` are absolute.
- `stop_signal` is a signal name vmsetup knows, and `stop_timeout` is a
  positive Go duration string.
- Every `env` entry is `KEY=value` with a non-empty key; interface names are
  unique.

A violation is a host-side bug and parks the VM with the field named.
`vmsetup check FILE` runs the same validation on a workstation, against a blob
or the bare JSON.

### 4.3 How `command` decides zpinit's mode

zpinit picks its mode from its own command line: a command means wrap, none
means supervise. vmsetup passes `handoff.command` through as that command line,
so the mode follows from the document with no extra field.

- **Wrap.** The Docker command runs as zpinit's one child, as `handoff.user`
  in `handoff.working_dir`. zpinit itself stays root, because it is PID 1 and
  must be able to reboot; vmsetup passes the user in `machine.json` (§5.6).
- **Supervise.** Users and directories are per service, in the image's own
  zpinit service TOML.

---

## 5. Bringup and handoff

Each step logs its failure clearly to the console and parks (§6.2).

### 5.1 Before the config: stdio, signals, `/proc`

- **`/dev` first.** The initramfs has no devtmpfs until vmsetup mounts one
  (§5.2), and both the console and the config device live there.
- **PID 1's stdio.** Production boots without `console=` (§7.2), so the kernel
  may hand init closed descriptors, and a process inheriting a closed fd 1 gets
  it assigned to the first file it opens. vmsetup points fd 0 at `/dev/null`
  and fds 1 and 2 at the log target. zpinit inherits that across the `execve`,
  and its children inherit it from zpinit.
- **Every catchable signal is caught.** For the few milliseconds vmsetup is
  PID 1, a signal the Go runtime has no `Notify` for would end the process, and
  PID 1 exiting panics the kernel. Caught signals are dropped. The `execve`
  resets every disposition, so nothing leaks into zpinit.
- **Logging.** `/dev/ttyS0` exists once devtmpfs is mounted. `internal/vmlog`
  buffers until the port opens, so nothing logged before that is lost.
- **`/proc`**, mounted in the initramfs so `/proc/cmdline` can be read, and
  moved into the new root in §5.2 rather than mounted twice.

### 5.2 Mount filesystems

#### The root disk, and switching into it

The kernel's root is the initramfs: a ramfs holding the init bundle and
nothing else. The VM's real root is `root.device`: a writable, per-VM copy of
the image that fctl attached. vmsetup mounts it and moves everything onto it:

```
check / is a ramfs or tmpfs                                    # refuse to run anywhere else
mkdir /newroot
mount(root.device, /newroot, root.fstype, 0, root.options)
assert it is mounted read-write
mkdir -p /newroot/{dev,proc,sys,run}                           # the disk is writable
mount("run", /newroot/run, "tmpfs", MS_NOSUID|MS_NODEV, "mode=0755")
mount tmpfs on /newroot/run/zpinit/bin; copy zpinit, zpctl in; remount it ro
MS_MOVE /dev  → /newroot/dev
MS_MOVE /proc → /newroot/proc
delete the initramfs's own files, not crossing into other filesystems
chdir /newroot; mount(".", "/", MS_MOVE); chroot("."); chdir("/")
```

- **`MS_MOVE` + `chroot`, not `pivot_root`.** The initramfs is the rootfs
  mount itself and cannot be unmounted, so `pivot_root(2)` refuses it. Moving
  the new root over `/` and chrooting into it is what busybox's and klibc's
  `switch_root` do.
- **The initramfs is freed.** It stays mounted underneath, and its unpacked
  size is guest RAM for the life of the VM, so its files are deleted before the
  move, without crossing into `/newroot`, `/dev` or `/proc`. Because that
  deletes every file on `/`, vmsetup first checks that `/` really is a ramfs or
  tmpfs and refuses otherwise. A vmsetup booted by hand from a disk root must
  fail there, not wipe the disk.
- **zpinit survives the switch on a read-only tmpfs.** After the chroot the
  initramfs is unreachable, so zpinit and zpctl are copied to
  `/run/zpinit/bin`, which is then remounted read-only. zpinit gets a real
  path, `zpctl` is runnable from an exec session, and the image's own
  `/usr/local/bin/zpinit`, if it has one, is never what runs (§9). The `/run`
  tmpfs from the table below is mounted here, during the switch, so the bin
  tmpfs can sit inside it; the table then skips `/run`, because stacking a
  second tmpfs there would hide it.
- **Missing mount points are created.** OCI images need not contain `/dev`,
  `/proc`, `/sys` or `/run`, and the root is writable.

If mounting the root fails, vmsetup parks *without* switching (§6.2). The
rescue machine is then the initramfs itself, with `/dev`, `/proc` and a `/run`
tmpfs.

#### The mount table

Into the new root, after the switch, ordered, mirroring fly.io's init
([init-snapshot](https://github.com/superfly/init-snapshot),
`src/bin/init/main.rs`):

| Mount | Type | Flags |
| --- | --- | --- |
| `/dev` | devtmpfs | `MS_NOSUID`, `mode=0755` |
| `/proc` | proc | `MS_NODEV\|MS_NOEXEC\|MS_NOSUID` |
| `/sys` | sysfs | same |
| `/dev/pts` | devpts | `MS_NOEXEC\|MS_NOSUID\|MS_NOATIME`, `mode=0620,gid=5,ptmxmode=666` |
| `/dev/shm` | tmpfs | `MS_NOSUID\|MS_NODEV`, mode 1777 |
| `/dev/mqueue` | mqueue | common flags |
| `/run` | tmpfs | `MS_NOSUID\|MS_NODEV`, `mode=0755`, mounted during the switch |
| `/sys/fs/cgroup` | cgroup2 | `MS_NODEV\|MS_NOEXEC\|MS_NOSUID\|MS_RELATIME` |

Then the symlinks: `/dev/fd → /proc/self/fd`, `/dev/stdin → /proc/self/fd/0`,
`/dev/stdout → /proc/self/fd/1`, `/dev/stderr → /proc/self/fd/2`.

`mkdir` failures with `EEXIST` are not errors. Mount **cgroup2 unified only**;
do not replicate fly's cgroup v1 hierarchy.

Then `mounts[]`, in order.

#### `/dev` and `/proc` are moved, then re-flagged

**In an initramfs the kernel does not mount devtmpfs for you.**
`CONFIG_DEVTMPFS_MOUNT` acts only when the kernel mounts a root device itself.
So vmsetup mounts devtmpfs on `/dev` as its very first act, and `/proc` next.
Both are moved into the new root, and the table's rows for them mean *apply
these flags* to the moved mount, with `MS_REMOUNT|MS_BIND`. That changes the
per-mount flags without touching the superblock, which matters for devtmpfs in
particular, being one kernel-wide instance.

`EBUSY` on any mount means "already mounted here": adopt it and apply the
flags. Treating `EBUSY` as "done" silently loses the flags: booted from a root
drive, where the kernel *does* automount devtmpfs, it lost `nosuid` on `/dev`
[VERIFIED — the boot test caught it].

> Docker's export leaves `/etc/resolv.conf`, `/etc/hosts`, `/etc/hostname` as
> **0-byte regular files** and `/dev/console` as a **0-byte regular file, not a
> device node**, even for `scratch`. devtmpfs over `/dev` shadows the fake
> console. The three `/etc` files are written in §5.4.

### 5.3 Network via netlink

Use `github.com/vishvananda/netlink`: pure Go, no cgo. **Never shell out to
`ip`**; `scratch` has no iproute2 and Alpine's busybox `ip` differs from
Debian's.

fctl gives every VM exactly one interface, but the schema keeps a list and the
code handles one or more. For each:

1. Find the link by MAC if `mac` is set, else by name. Firecracker does not
   guarantee NIC enumeration order matches API call order.
2. `LinkSetUp`
3. `AddrAdd` the address
4. `RouteAdd` the default route via `gateway`, **with `RTNH_F_ONLINK` when the
   address does not cover the gateway** (below)
5. If `mmds: true`, `RouteAdd` a host route to `169.254.169.254` on this link

Always bring `lo` up. Failing to do so breaks a surprising amount of software.

#### The default route needs `onlink` under a /32 [VERIFIED]

fctl allocates each VM **one routed address with no on-link subnet at all**: a
bare `/32` whose gateway is a link-local address that every tap on the host
carries. That costs one address per VM instead of the four a `/30` burns, and
the gateway is the same for every VM.

With a `/32` the gateway is inside no connected prefix, so a plain route add
fails and the VM boots with no default route. Verified by disabling the flag
and re-running the `routed32` boot scenario:

```
configured eth0 with 10.77.0.5/32
error: bringup failed: interface eth0: add default route
       via 169.254.0.1 on eth0: network is unreachable
```

`RTNH_F_ONLINK` tells the kernel to treat the nexthop as directly reachable,
which it genuinely is: the tap is point-to-point with the host.

**The flag is computed, not configured.** `netconf.gatewayNeedsOnlink` sets it
only when the interface's own address does not contain the gateway. A field
would be one more thing fctl and vmsetup can disagree about. Setting it
unconditionally would be worse: on a conventional subnet a mistyped gateway
fails at route-add time, loudly, and with the flag always on it would install
a route that silently blackholes instead.

### 5.4 Identity files

- `sethostname(hostname)` and write `/etc/hostname`
- Write `/etc/hosts`: `127.0.0.1 localhost`, `::1 localhost`, the VM's own
  address → hostname, then `network.extra_hosts`
- Write `/etc/resolv.conf` from `network.nameservers` and `search`

Truncate and rewrite rather than append: the files exist but are empty, and
re-running must be idempotent. These writes land on the VM's own disk and are
rewritten on every boot.

### 5.5 Resolve users and compose env

Resolve `handoff.user` (wrap mode) and `exec.user` in `user[:group]` form,
against the new root's `/etc/passwd` and `/etc/group`. Accept both names and
numeric ids; a numeric uid absent from `/etc/passwd` is valid, with `HOME=/`.
A name that does not resolve parks the VM with `step=user`. zpinit resolves
the same names again when it switches to them; resolving them here first makes
a typo fail at boot, naming the field.

> Build with `CGO_ENABLED=0`. Go's `os/user` then uses its pure-Go
> implementation, which parses `/etc/passwd` directly, and does not honour NSS
> (LDAP etc.), which is correct for a container image.

`env` arrives final. vmsetup adds only what it alone can know:

- `HOME`, from `handoff.user` in wrap mode or root in supervise mode, if not
  already set
- `TERM=linux`, if not set

These are defaults, not a merge: a value fctl sets always wins.

**`/run/zpinit/env`.** vmsetup also writes the composed env there, in
`KEY=value` lines, mode `0600` (image env routinely carries credentials).
zpinit merges that file above its container-env tier before every
`entrypoint.d` script. Its contents equal `envp`, so the merge changes no value;
what it adds is visibility, since scripts read and append to the file to hand
variables forward. The format cannot represent a newline in a value or an
unusual key, so such a variable is left out of the file with a warning and
still reaches zpinit through `envp`.

### 5.6 Handoff: write the machine settings, then `execve` zpinit

vmsetup's last act is `execve`. zpinit replaces it in place and inherits
**PID 1**, stdio and the working directory. Nothing of vmsetup stays resident.

Bringup and being PID 1 are different jobs. Docker splits them: runc prepares
the container and exits, and tini (or zpinit) is PID 1. vmsetup is the runc
half; zpinit, with machine mode, is the tini half, plus the few things only the
kernel's init has to do. In machine mode zpinit never exits: every path ends in
kill, sync, unmount, reset line and reboot, because PID 1 exiting panics the
kernel. Machine mode is detected, not configured: PID 1 **and** a PID namespace
whose link reads `pid:[4026531836]`, the fixed inode of the kernel's initial
namespace [VERIFIED in the guest, docs/RESEARCH.md §6]. So a container can
never enter it by accident.

#### What vmsetup hands over

| Channel | Carries |
| --- | --- |
| the binary | `/run/zpinit/bin/zpinit`, the bundle's copy on a read-only tmpfs (§5.2). Unswitched (rescue before the root mounted): `/bin/zpinit` in the initramfs |
| `argv` | `[zpinit]` in supervise mode, `[zpinit, "--", command…]` in wrap mode. In rescue: `--skip-entrypoint --config <empty dir>` (§6.2) |
| `envp` | the composed env (§5.5): zpinit's container-env tier, as under Docker |
| cwd | `handoff.working_dir` in wrap mode, `/` otherwise |
| stdio | fd 0 `/dev/null`, fds 1–2 the log target (§5.1) |
| `/run/zpinit/env` | the composed env again, for `entrypoint.d` (§5.5) |
| `/run/zpinit/machine.json` | what only the VM side knows (below) |

`/run/zpinit/machine.json`, mode `0600`. Its format is zpinit's
([zpinit#10](https://github.com/0ploy/zpinit/issues/10)); vmsetup is its only
writer:

```jsonc
{
  "v": 1,
  "console": "/dev/ttyS0",          // where zpinit writes the reset line, even with logging off
  "stop": { "signal": "SIGQUIT",    // handoff.stop_signal, sent to the wrap-mode child
            "timeout": "10s" },     // handoff.stop_timeout, the whole graceful stop
  "user": "nginx",                  // handoff.user: the wrap-mode child's identity. Absent in supervise mode
  "exec": {                         // absent → no listener
    "port": 1024, "user": "nginx", "working_dir": "/",
    "env": ["PATH=…", "…"]          // the composed env, the base for every exec
  },
  "rescue": { "step": "network",    // present only when parked, §6.2
              "error": "…" }
}
```

zpinit reads this file only in machine mode, and a container that copies one
in gets a warning and nothing else. That is what makes it safe for the file to
switch on a vsock listener.

### 5.7 Ending the VM — zpinit's, stated here because fctl matches it

vmsetup never ends the VM in the normal path. zpinit does, in machine mode:

- **Graceful stop.** fctl's `SendCtrlAltDel` arrives as `SIGINT` to PID 1,
  because zpinit called `CAD_OFF`. In wrap mode zpinit sends `stop.signal` to
  the child and `SIGKILL`s at `stop.timeout`; in supervise mode it runs its
  ordered stop within the same budget. `stop_timeout` must be shorter than
  fctl's own stop timeout, and fctl composes both.
- **A Ctrl+Alt+Del sent too early is lost** [VERIFIED]. The initramfs boot is
  fast enough that one sent ~0.65 s after the VMM starts can arrive before the
  guest's i8042/atkbd driver has finished probing, and nothing happens. A
  resend 1 s later works. So a stop must be resent while the VM is still up,
  as `test/boot.sh` does.
- **The workload ends on its own**: in wrap mode the child exits, in supervise
  mode whatever ends zpinit in a container. Either way the VM is done, exactly
  as a container is done; whether it comes back is fctl's restart policy.
- **Teardown:** `kill(-1, SIGKILL)` and reap, `sync()`, remount `/` read-only
  (zpinit runs from the bin tmpfs, so it holds nothing open on the disk),
  unmount the rest best effort, the reset line, `reboot(RB_AUTOBOOT)`. With
  `reboot=k` (§7.2) that exits the VMM.

#### The reset line

Immediately before `reboot()`, zpinit writes one line to the console saying
*why*, because a guest reset is the same event to the host whether the guest
meant "restart me", "I am done", or panicked under `panic=1`:

```
zpinit: reset intent=shutdown cause=exit status=0
zpinit: reset intent=shutdown cause=exit signal=SIGSEGV
zpinit: reset intent=shutdown cause=stop status=143
zpinit: reset intent=shutdown cause=parked
```

- Always its own line, with a leading `\n` so a partial line from the workload
  cannot swallow it. fctl matches lines beginning `zpinit: reset `.
- **Lines arrive as `\r\n`**: the serial tty's `ONLCR` translates every `\n`.
  Strip the `\r` before matching [VERIFIED — the boot test's first matcher
  failed on exactly this].
- `cause=exit`: the workload ended by itself. `cause=stop`: a graceful stop
  was requested. `cause=parked`: a parked VM was stopped. `status=` or
  `signal=` is the exit zpinit would have reported in a container.
- A protocol line, not a log line: written even with `logging.target = "none"`.
- `intent=reboot` has no producer yet; it is reserved so a matcher handles it
  from the start.
- An absent line means "unknown": a panic, a guest calling `reboot(2)` itself,
  or a Ctrl+Alt+Del during vmsetup's bringup (§3). fctl's restart rate-limiter
  is the backstop there.

**It is forgeable, and that is fine.** The guest owns its disk and can print
anything on its own serial port. Forging `shutdown` buys a stop it could get by
exiting; forging `reboot` buys a restart it could get by rebooting. The
corollary: **this channel must never carry anything the host acts on more
strongly than this.**

---

## 6. `exec`, parking and rescue

### 6.1 `exec` is zpinit's

`fctl exec`, the microVM's `docker exec`, is served by zpinit in machine mode.
vmsetup's only part is to validate `exec` in the config and copy it, with the
composed env, into `machine.json`. The requirements and the draft wire protocol
are zpinit#10 item 8. In outline:

- **Transport.** fctl attaches a vsock device and connects through
  Firecracker's UDS muxer (`CONNECT <port>`); zpinit sees an `AF_VSOCK` stream
  on `exec.port`.
- **Host only.** The host never listens, so a guest-initiated vsock connection
  reaches no host process. zpinit mirrors that by accepting only peer CID
  `VMADDR_CID_HOST` (2).
- **Never in a container.** The listener needs both machine mode and
  `machine.json`. vsock is not network-namespaced up to 6.18, and the
  namespace support added later defaults to `global`, so a container on a host
  that is itself a VM must not open a listener the hypervisor can reach.
- **Semantics.** One exec per connection; the env is the composed env plus the
  request's (`docker exec` semantics); the default user is `exec.user`; a
  dropped connection `SIGKILL`s the exec's process group.

### 6.2 Parking: don't die, hand a broken machine to rescue

When any step fails, vmsetup **parks**:

1. logs the error
2. writes the park line, its own line on `/dev/ttyS0`, under the same rules as
   the reset line:

   ```
   vmsetup: parked step=network error="interface eth0: no link with mac 06:00:DE:AD:BE:EF"
   ```

   `step` is one of `config`, `root`, `mounts`, `network`, `identity`, `user`,
   `handoff`. fctl surfaces parked distinctly from running and exempts it from
   restart policy.
3. makes sure `/run` is a tmpfs in whatever root it is in (the new root if the
   switch happened, otherwise the initramfs), writes `machine.json` with
   `rescue` set, plus `exec` if the config was read, and creates an empty zpinit
   config directory
4. `execve`s zpinit with `--skip-entrypoint --config <empty dir>`, so neither
   the image's entrypoint scripts nor its services run, and a broken image
   config cannot also break rescue

zpinit then idles as PID 1 with its stop handling and, when configured, the
exec listener. No retry and no workload. The machine is broken; it exists to
be looked at over `fctl exec` and then destroyed. A graceful stop ends it with
`zpinit: reset intent=shutdown cause=parked`.

Exec into a parked machine sees whatever bringup reached: if the root disk
never mounted, the root is the initramfs; if the network failed, there is no
address. No address, no route, no sshd and no shell in the image are needed to
get in. A `scratch` image has no shell to exec, so rescue there means exec'ing
a static debugging binary, which is fctl's to provide.

**If zpinit itself cannot be exec'd** (a bundle built wrong), there is nothing
to hand the machine to. vmsetup writes `vmsetup: parked step=handoff
error="…"`, calls `sync()` and `reboot(RB_AUTOBOOT)`, and with `reboot=k` the
VMM exits. It does not stay to reap or wait for a stop, because that would be
process management. fctl sees the park line followed by the VM ending, and
treats it as a failed boot.

---

## 7. What vmsetup needs from the kernel and cmdline

The guest kernel and the command line belong to fctl. This section states only
what vmsetup *depends* on, so a change there that breaks vmsetup is
recognisable as such.

### 7.1 Kernel options vmsetup relies on

```
CONFIG_BLK_DEV_INITRD=y                 # the init bundle, §3. without it: no /init, no root, panic
CONFIG_VIRTIO_BLK=y  CONFIG_VIRTIO_NET=y  CONFIG_VIRTIO_MMIO=y
CONFIG_EXT4_FS=y
CONFIG_DEVTMPFS=y                       # vmsetup mounts it itself, §5.2
CONFIG_SERIAL_8250=y  CONFIG_SERIAL_8250_RUNTIME_UARTS=1
                                        # logging, the park and reset lines
CONFIG_SERIO_I8042=y  CONFIG_KEYBOARD_ATKBD=y
                                        # Ctrl+Alt+Del must reach zpinit, §5.7
CONFIG_VSOCKETS=y  CONFIG_VIRTIO_VSOCKETS=y   # exec and rescue, zpinit's (§6.1)
```

> The i8042 pair is **not** in Firecracker's checked-in
> `resources/guest_configs/*.config` files; it comes from `ci.config`, which
> the build script concatenates on top. Build from the base config alone and
> `SendCtrlAltDel` silently stops working, which takes graceful stop with it.

### 7.2 Cmdline vmsetup assumes

The production line is `vmsetup_config=/dev/vda quiet reboot=k panic=1
nomodule`, with no root device attached, so Firecracker appends no `root=`
(it does append its own `pci=off`). Of that, the guest depends on:

- **No `init=` and no `root=`.** The initramfs's `/init` is vmsetup, and the
  kernel runs it without being told to. An `init=` would name a path *inside
  the initramfs*, and a `root=` would only matter if `/init` failed, which must
  panic rather than boot the workload's disk (§3).
- **`vmsetup_config=<device>`** (§4.1). Missing is a park.
- **`reboot=k`**: zpinit ends the VM by `reboot()` (§5.7), which must exit the
  VMM.
- **`panic=1`**: a failed `/init` or a panic resets instead of hanging.
- **No `console=` in production.** vmsetup logs to `/dev/ttyS0` directly, and
  zpinit writes the reset line there. The port is enumerated with or without
  `console=` on x86_64 [VERIFIED], and without it no getty contends for it.
- **Never `i8042.nopnp`**: it breaks `SendCtrlAltDel`, and with it graceful
  stop.
- **No `ip=`, ever.** A `/32` cannot be expressed through it: the kernel's
  `ipconfig` initialises its netmask to `INADDR_NONE`, bit-for-bit
  `255.255.255.255`, so a `/32` is indistinguishable from "unset". vmsetup
  configures the one interface, and a VM whose network bringup failed is
  reached over `exec` (§6.2), not a debug NIC.

---

## 8. Images and the init bundle

### 8.1 What the image must be, and how fctl derives `handoff` from it

The image is an OCI image's filesystem as a plain ext4, with nothing of ours in
it: no guest init, no config file, no injected mount points. fctl builds it and
composes the config from the OCI image config; vmsetup never reads the OCI
config. The rules fctl applies, so the two sides agree:

- **A plain Docker image is wrap mode.** `command` is `Entrypoint + Cmd`,
  `user` is `User` (or `root`), `working_dir` is `WorkingDir` (or `/`),
  `stop_signal` is `StopSignal` (or `SIGTERM`). zpinit runs the command as its
  one child and stays PID 1, which is `docker run --init`.
- **A zpinit-native image** (its entrypoint is a zpinit binary) has the
  bundle's zpinit take that entrypoint's place. With no remaining command it is
  **supervise** mode: no `command`, `user` or `working_dir`, and the image's own
  `/etc/zpinit` is the configuration.
- **Per-launch overrides.** In wrap mode, `--entrypoint`, `--user`, `-w` and a
  command override apply to `handoff` exactly as Docker would. In supervise
  mode they live in the image's service TOML, which vmsetup never reads, so
  they must be refused rather than half-applied. Per-launch `env` is fine in
  both modes; it reaches zpinit through `envp`.
- Docker's export leaves `/.dockerenv`, 0-byte `/etc/{resolv.conf,hosts,
  hostname}` and a fake `/dev/console` regular file. vmsetup copes with all of
  them (§5.2, §5.4); removing `/.dockerenv` is the image builder's job.

### 8.2 The init bundle

Built by `cmd/mkbundle`, attached by fctl as `initrd_path` on every boot. An
**uncompressed newc cpio**, because Firecracker copies it into guest memory
as-is and decompression is boot time spent for nothing:

```
/init                   vmsetup, static, CGO_ENABLED=0
/bin/zpinit             zpinit, with machine mode (zpinit#10)
/bin/zpctl
/dev/console            c 5 1 — the kernel opens it for /init's stdio before vmsetup runs
/dev/  /proc/  /newroot/  /run/   empty mount points
```

- **Reproducible.** Sorted entries, mtime 0, uid/gid 0, inode numbers by
  position, nothing taken from the host but file contents. The same vmsetup and
  zpinit binaries give a byte-identical cpio, so fctl can content-address it.
- **Its unpacked size is guest RAM** until vmsetup frees it after the switch
  (§5.2): about 9 MiB for three static Go binaries.
- An image that carries its own zpinit runs under the bundle's instead, so
  zpinit's configuration format must stay backwards compatible, which as a
  shipping product it promises anyway.

---

## 9. Security model

The workload is untrusted and has full control of its own disk and guest
kernel state. Therefore:

- **The VM boundary is the only boundary.** vmsetup enforces nothing; all
  limits are host-enforced.
- **The workload owns its disk but not PID 1.** The root is writable and
  persists across restarts, so anything baked into it could be overwritten and
  would own the next boot, and with it the stop, the reset line and the rescue
  and exec paths the host relies on. From the init bundle, what runs as init is
  the host's choice on every boot. After the switch, zpinit runs from a
  read-only tmpfs copy (§5.2), never from the disk. That is guest-local either
  way, since a workload that owns the guest crosses nothing by replacing its
  own init; but the host's lifecycle depends on those binaries behaving.
- **The boot fails closed** (§3). There is no root device, so a broken `/init`
  panics instead of booting the workload's disk.
- **Freeing the initramfs refuses to run on a disk root** (§5.2).
- **exec accepts only the host CID, and exists only in machine mode** (§6.1).
- **The serial lines are forgeable by design** (§5.7) and carry nothing the
  host acts on strongly.
- **MMDS, when attached, is guest-readable by definition** and must never carry
  host-side secrets. If an MMDS consumer is ever built it is V2 only, with no
  fallback to V1.

---

## 10. Interfaces

| Interface | Direction | Owned by | Here |
| --- | --- | --- | --- |
| Kernel cmdline: `vmsetup_config=` (never `init=`, `root=`, `ip=`) | host → guest | fctl | §7.2 |
| The init bundle, as `initrd_path` | host → guest | built here, attached by fctl | §8.2 |
| The config blob on `/dev/vda` | host → vmsetup | schema here, written by fctl | §4 |
| The park line `vmsetup: parked …` | vmsetup → host | here | §6.2 |
| `/run/zpinit/machine.json`, `/run/zpinit/env`, argv, `envp`, cwd | vmsetup → zpinit | formats zpinit's | §5.5, §5.6 |
| The reset line `zpinit: reset …` | zpinit → host | zpinit | §5.7 |
| vsock exec and rescue | host ↔ zpinit | zpinit's protocol, fctl's device | §6.1 |

---

## 11. Testing

```
go test ./...                     # unit tests, no KVM needed
sudo KERNEL=/path/to/vmlinux ZPINIT=/path/to/zpinit test/run.sh [scenario...]
```

`test/run.sh` builds a fixture per scenario, boots it under Firecracker exactly
as fctl does (the init bundle as `initrd_path`, no root device, the config blob
`/dev/vda`, the image `/dev/vdb`) and judges it **from the host**. It waits for
a scenario-specific line on the serial log, performs the scenario's action
(Ctrl+Alt+Del, resent while the VM is up, or nothing), waits for the VM to end
by itself, and then checks the probe's report, the park and reset lines, and the
absence of a kernel panic. `build-rootfs.sh` generates the bundle, the image,
the config (encoded by `test/mkblob` with vmsetup's own encoder, after
validating it with vmsetup's own parser), the probe's expectations and the
host's TAP parameters from one set of shell variables, so none of them can
drift.

The image is deliberately `scratch`-like: static Go binaries, no shell, no
libc, no guest init. It carries a stub `/usr/local/bin/zpinit` that fails the
run if it is ever executed, standing in for a zpinit-native image's own copy.

| scenario | proves |
| --- | --- |
| `base` | supervise mode: the probe as a zpinit service checks the root is the writable disk, `/run/zpinit/bin` is a read-only tmpfs, `/proc/1/exe` is the bundle's zpinit, the initramfs was freed, `machine.json` and `/run/zpinit/env` are mode 0600, the mount table and its flags, network, identity, env, user, the process chain `probe → zpinit(1)` and the initial PID namespace, and the host's fetch over the network |
| `routed32` | `base` on the routed `/32` with the `RTNH_F_ONLINK` default route (§5.3) |
| `park` | a MAC no NIC has: `parked step=network`, zpinit idles in rescue, the VM stays up until stopped |
| `noroot` | a root device that does not exist: `parked step=root`, zpinit idles in the initramfs |
| `nozpinit` | a bundle without zpinit: `parked step=handoff`, then the VM ends at once, with no kernel panic and no reset line from vmsetup |
| `blob-magic`, `blob-version`, `blob-noparam` | a wrong magic, a version-2 header and a missing `vmsetup_config=` each park with `step=config` and the reason |
| `command` | wrap mode: the probe as `handoff.command`, as uid 1000, child of zpinit(1); it exits → `cause=exit status=0` |
| `stop`, `stopkill` | wrap-mode stop: `stop_signal`, then `SIGKILL` at `stop_timeout` |

All but the last three pass [VERIFIED — Firecracker v1.17.0, guest kernel
6.18.39]. `command`, `stop` and `stopkill`, and the reset line in every
scenario, need zpinit's machine mode; `run.sh` skips and reports them until
`ZPINIT_MACHINE_MODE=1` is set. Measured from the host, the probe reports
~0.43 s after the Firecracker process starts; vmsetup's own bringup, `/init`
start to `execve`, takes 21–35 ms, of which the switch root is ~8 ms.

**The network proof is host-initiated.** The probe listens and the host
fetches `/ping` from the guest's address. A host firewall that drops
guest-initiated connections cannot then fail the test, and the proof is the
same: the reply has to leave over the guest's address and routes, and under
`routed32` the reply to the host's link-local address can only leave via the
onlink default route.

Unit tests cover blob parsing and every rejection, schema validation, the
`machine.json` and env-file writers, env defaulting, user resolution including
numeric ids and missing `/etc/passwd`, mount option parsing, the onlink
decision, and the bundle's layout and reproducibility (cross-checked against
GNU `cpio` when installed).

Boot time is measured **from the host**, never with in-guest tooling: guest
clocks are unreliable during early boot.

---

## 12. Deferred

Not now, but nothing here should be designed against:

- **aarch64.** There is no i8042, so graceful stop needs a different trigger,
  and the serial device is MMIO and *is* conditional on `console=`.
- **An MMDS agent** for live config updates. MMDS is off by default on the host
  (it costs a check on every transmitted frame and its traffic bypasses the
  rate limiter), and push over exec does the job better. If built, it lives in
  zpinit's machine mode and polls V2 only.
- **`intent=reboot`** (§5.7) gets a producer when there is a concrete in-guest
  reboot request to honour.
- **A DAX lower plus overlay.** An image on virtio-pmem mounted `ro,dax`, a
  writable upper on virtio-blk, and overlayfs between them, measured by fctl to
  cut pinned host memory from ~448 MB to ~82 MB per VM at 16 VMs. It is deferred
  until real workloads show guest page cache dominating. It would slot into the
  switch (§5.2) without touching the initramfs boot: mount the lower and the
  upper, assemble the overlay at `/newroot`, switch as now. What vmsetup would
  then need:
  - an `overlay_root` block in place of `root`: the lower `{device, fstype,
    options: "ro,dax"}`, the upper `{device, fstype}` or tmpfs, and `upperdir`
    and `workdir` *relative to the upper*, so they cannot name different
    filesystems, distinct and non-nested;
  - **an assertion that `dax=always` and `ro` are in effect**, read from
    `/proc/self/mountinfo`, parking with `step=overlay` when missing. Losing DAX
    is silent: no error, only density collapsing at scale. And a write to a
    read-only pmem device on x86_64 is *discarded* by Firecracker rather than
    faulted.
  - the reason the overlay exists at all: a writable DAX root is 19× slower on
    small-file writes and fragments the image, so reads go to the DAX lower and
    writes to the virtio-blk upper.
- **Snapshots, and VMGenID.** Restoring N VMs from one snapshot duplicates
  machine ids, entropy pools, RNG seeds and pre-snapshot key material.
  Something resident has to react to a VMGenID change by re-seeding and
  regenerating what is assumed unique. vmsetup is never resident, so that would
  be zpinit's machine mode. Snapshots stay off until that path exists.

---

## 13. Remaining work

- **Once zpinit's machine mode lands** (zpinit#10): run the wrap-mode scenarios
  and require the reset line (`ZPINIT_MACHINE_MODE=1`), then make that the
  default. Add `supervise` (zpinit restarts a service, and a service exit does
  not end the VM), `orphan` (a double-forked process is reaped by zpinit(1)),
  and `persist` (a write just before the workload exits, or before a graceful
  stop, survives into the next boot).
- **Once zpinit's exec listener lands**: `exec` (exit status, PTY and resize,
  stdin EOF, a dropped connection kills the process group, a non-host peer CID
  is refused) and `rescue-exec` (exec into each parked case).
- **Boot-timer signal**: report vmsetup's `/init`-to-`execve` interval by
  writing Firecracker's boot-timer device just before the `execve`.

---

## 14. Open questions

1. **Schema sharing.** fctl writes the config and vmsetup reads it. If fctl
   imports vmsetup's config types (moved out of `internal/` into a public
   package), writer/reader drift becomes a compile error. The same applies to
   `machine.json`, whose types would then live in zpinit.
2. **Line prefixes.** `vmsetup: parked` and `zpinit: reset` are to be agreed
   with fctl before either ships.
3. **Rescue invocation.** §6.2 uses `--skip-entrypoint --config <empty dir>`
   plus `rescue` in `machine.json`. Whether zpinit needs the flags or derives
   everything from `rescue` is zpinit#10's call.
4. **Who writes the service TOML for supervise mode.** Today the image carries
   it. fctl composing it per launch, and vmsetup writing it at boot, would make
   per-launch overrides possible in supervise mode too, at the cost of a field
   in §4.2.
5. **Bundle releases.** One bundle per (vmsetup, zpinit) version pair; who cuts
   it, and how fctl selects one.
6. **Exec protocol details** (§6.1), zpinit's to fix with fctl's client:
   whether a request may ask for its process to survive the connection
   (current answer: no), and whether exec needs a concurrency limit.
7. **zpinit's stop budget.** Its supervise-mode shutdown budget for a default
   config was observed at 38 s, which a short `stop_timeout` would cut off.
