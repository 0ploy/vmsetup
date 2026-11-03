# vmsetup — spec

vmsetup is `/init` of an initramfs the host attaches on every boot. It makes
the VM bootable and then **always `execve`s
[zpinit](https://github.com/0ploy/zpinit)**, which is PID 1 for the life of the
VM. vmsetup manages no process at any point: no fork, no reaping, no stop
handling. Everything after the `execve` is zpinit's **machine mode**
([zpinit#10](https://github.com/0ploy/zpinit/issues/10)).

This document specifies what other components depend on: the config fctl
writes, what the guest looks like when zpinit starts, what vmsetup hands
zpinit, the lines fctl matches on the serial port, and what vmsetup needs from
the kernel and the image. How it does it is in the code. The evidence behind
the decisions is in [docs/RESEARCH.md](docs/RESEARCH.md), what is
deliberately not built yet is in [docs/future.md](docs/future.md), and open
work is in the [issues](https://github.com/0ploy/vmsetup/issues).

**Target platform:** x86_64, Linux guest kernel 5.10+.

## 1. Interfaces

| Interface | Direction | Owned by | Section |
| --- | --- | --- | --- |
| Kernel cmdline: `vmsetup_config=` (never `init=`, `root=`, `ip=`) | host → guest | [fctl](https://github.com/0ploy/fctl) | §9 |
| The init bundle, as `initrd_path` | host → guest | built here, attached by fctl | §10.2 |
| The config blob on `/dev/vda` | host → vmsetup | schema here, written by fctl | §4 |
| The park line `vmsetup: parked …` | vmsetup → host | here | §7 |
| argv, `envp`, cwd, stdio, `/run/zpinit/machine.json`, `/run/zpinit/env` | vmsetup → zpinit | formats zpinit's | §6 |
| The reset line `zpinit: reset …` | zpinit → host | zpinit | §8 |
| vsock exec and rescue | host ↔ zpinit | zpinit's protocol, fctl's device | zpinit#10 |

## 2. Principles

1. **Bringup, not process management.** vmsetup is runc's half of a container
   runtime, not tini's. It is PID 1 only because the kernel starts it there,
   and zpinit inherits PID 1 through the `execve`. Exactly one process manager
   runs per VM.
2. **zpinit stays free of OCI and hypervisor knowledge.** Image-derived
   settings reach it through argv, `envp` and its own config; VM-specific ones
   through `machine.json`, which zpinit reads only in machine mode.
3. **No dependency on the guest image.** Direct syscalls from a static binary
   the host supplies. vmsetup never shells out: `scratch` images have no
   userspace.
4. **Enforce on the host, never in the guest.** The workload is untrusted; all
   limits are Firecracker's and the host's.
5. **Fail loudly, never silently, and never by dying.** A misconfigured VM that
   boots anyway is worse than one that refuses with a clear message. Refusing
   means parking (§7).
6. **No merging in the guest.** fctl writes the config and vmsetup reads it,
   and drift between them is the likeliest bug in the design. So every field
   arrives resolved, unknown fields are rejected, and vmsetup defaults only
   what it alone can know.

## 3. Boot sequence

```
Firecracker   initrd_path=init.cpio, no root device
  └─ kernel   vmsetup_config=/dev/vda reboot=k panic=1 …   (no init=, no root=)
       └─ /init = vmsetup, in the initramfs          ← PID 1, ~20–35 ms
            mount /dev and /proc, stdio, read and validate the config      §4
            ── any failure below → park (§7) ──
            mount root.device, switch root into it, the mount table        §5
            network, identity files, users, env                            §5
            write /run/zpinit/{env,machine.json}, execve zpinit            §6
       └─ zpinit                                     ← PID 1 for the life of the VM
```

There is no root device and no `init=`. If `/init` cannot be executed the
kernel looks for `root=`, finds none and panics, and `panic=1` resets: the boot
**fails closed**, and nothing on the workload's disk is ever booted by
accident.

vmsetup does not call `reboot(CAD_OFF)`; that is zpinit's first act. A
Ctrl+Alt+Del during vmsetup's milliseconds resets the VM on the spot, which is
an acceptable answer to a stop that early.

## 4. Configuration

### 4.1 The blob

One JSON document, composed in full by fctl for each launch, delivered as raw
bytes on a dedicated read-only 64 KiB block device. No filesystem, and no
config in the image.

```
offset  0   "FCTLCFG\0"        8 bytes, magic
offset  8   version            u32 little-endian; this document describes 1
offset 12   length             u32 little-endian, bytes of JSON that follow
offset 16   JSON document      NUL-padded to the end of the device
```

The cmdline names the device: `vmsetup_config=/dev/vda`. The name has no dot,
because dotted parameters are module parameters and never reach init. vmsetup
opens exactly that device and parks with `step=config` on a missing parameter,
a wrong magic, an unknown version, a `length` past the device, or JSON that
does not decode. It never searches elsewhere: a drive-ordering bug must read
as "not the config device", not as a parse of somebody's data volume. Every
other device is named in the document.

**Every device is waited for, bounded.** With an initramfs nothing waits for
device probing before `/init` runs, so vmsetup polls for the config device,
`root.device`, each `mounts[].device` and each interface for up to 2 s. One
that never appears parks with its step (`config`, `root`, `mounts`, `network`)
and says it did not appear. A device that is already there costs nothing.

Unknown fields are an error. The version is bumped for any change vmsetup
cannot read compatibly.

**The schema is code, shared.** The Go package
[`github.com/0ploy/vmsetup/config`](config) holds the types, the validation
below and the blob encoder. fctl builds a `config.Config` and writes
`cfg.Blob()`, so it runs the guest's validation at create time and a schema
change is a compile error in fctl, not a parked VM. A config built against one
version of the package belongs with a bundle whose vmsetup reads that schema;
pairing them is part of the bundle's provenance
([#6](https://github.com/0ploy/vmsetup/issues/6)).

### 4.2 Schema

```jsonc
{
  "hostname": "web-01",

  // What zpinit runs. Fully resolved by fctl (§10.1).
  "handoff": {
    // Entrypoint + Cmd. Present → zpinit wrap mode. Absent → supervise mode,
    // and user/working_dir must be absent too (§4.4).
    "command":      ["/docker-entrypoint.sh", "nginx", "-g", "daemon off;"],
    "user":         "nginx",          // user[:group], names or numeric ids
    "working_dir":  "/",
    "stop_signal":  "SIGQUIT",        // sent to the command on graceful stop
    "stop_timeout": "10s"             // zpinit's whole stop budget; below fctl's own
  },

  // The composed environment, final. KEY=value, order significant.
  "env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
          "NGINX_VERSION=1.31.3"],

  "network": {
    "interfaces": [
      {
        "name":    "eth0",
        "mac":     "06:00:AC:10:00:02",   // optional; find the link by MAC when set
        "address": "10.77.0.5/32",
        "gateway": "169.254.0.1",
        "mmds":    false,                 // add a host route to 169.254.169.254
        "mtu":     1480                   // optional; absent leaves the driver's MTU
      }
    ],
    "nameservers":  ["1.1.1.1"],
    "search":       [],
    "extra_hosts":  [{ "ip": "10.0.0.5", "host": "db.internal" }]
  },

  // The VM's disk, mounted read-write and switched into (§5).
  "root": { "device": "/dev/vdb", "fstype": "ext4", "options": "discard" },

  // Data volumes, mounted in order after the standard table.
  "mounts": [
    { "device": "/dev/vdc", "path": "/data", "fstype": "ext4", "options": "relatime" }
  ],

  // Optional. Absent → zpinit opens no exec listener. Passed through (§6).
  "exec": { "port": 1024, "user": "nginx", "working_dir": "/" },

  "logging": { "target": "serial" },  // "serial" | "none"

  // Optional. True → write Firecracker's boot-timer device just before the
  // execve (§6). Set it exactly when the VMM runs with --boot-timer.
  "boot_timer": true
}
```

### 4.3 Validation

Nothing is defaulted, so every field anything acts on is required. Before
anything acts on the document:

- `root.device` and `root.fstype` are set; `root.options` does not contain `ro`.
- `handoff.stop_signal` and `stop_timeout` are always set; `user` and
  `working_dir` exactly when `command` is; `command`, when present, is
  non-empty.
- `logging.target` is set, and so are all three fields of `exec` when present.
- `root.device`, the `working_dir`s and every `mounts[].path` are absolute.
- `stop_signal` is a known signal name; `stop_timeout` is a positive Go
  duration.
- Every `env` entry is `KEY=value` with a non-empty key; interface names are
  unique; an interface `mtu`, when set, is within 68–65535.

A violation is a host-side bug and parks with the field named.
`vmsetup check FILE` runs the same validation on a blob or bare JSON.

### 4.4 How `command` selects zpinit's mode

zpinit picks its mode from its own command line, and vmsetup passes
`handoff.command` through as that command line.

- **Wrap.** The command runs as zpinit's one child, as `handoff.user` in
  `handoff.working_dir`. zpinit itself stays root, because as PID 1 it must be
  able to reboot.
- **Supervise.** Users and directories are per service, in zpinit's service
  config.

## 5. The guest zpinit starts in

**Root.** `/` is `root.device`, mounted read-write with `root.options`. The
initramfs's files are gone: they would otherwise be guest RAM for the life of
the VM. vmsetup only frees them after checking `/` is a ramfs or tmpfs, so a
vmsetup booted by hand from a disk root fails there instead of wiping the
disk. Mount points the image lacks (`/dev`, `/proc`, `/sys`, `/run`) are
created.

**Mounts**, in this order, then `mounts[]`:

| Mount | Type | Flags |
| --- | --- | --- |
| `/dev` | devtmpfs | `nosuid`, `mode=0755` |
| `/proc` | proc | `nodev,noexec,nosuid` |
| `/sys` | sysfs | `nodev,noexec,nosuid` |
| `/dev/pts` | devpts | `noexec,nosuid,noatime`, `mode=0620,gid=5,ptmxmode=666` |
| `/dev/shm` | tmpfs | `nosuid,nodev`, mode 1777 |
| `/dev/mqueue` | mqueue | `nodev,noexec,nosuid` |
| `/run` | tmpfs | `nosuid,nodev`, `mode=0755` |
| `/run/zpinit/bin` | tmpfs, **read-only** | zpinit and zpctl from the bundle |
| `/sys/fs/cgroup` | cgroup2, unified only | `nodev,noexec,nosuid,relatime` |

Plus `/dev/fd`, `/dev/stdin`, `/dev/stdout` and `/dev/stderr` as symlinks into
`/proc/self/fd`.

**Network.** `lo` is up. Each interface is found by `mac` if set, else by
name, given `mtu` if set, then brought up with its address and a default
route via `gateway`. The
route carries `onlink` exactly when the address does not contain the gateway,
as with a routed `/32`. That is computed, not configured: always setting it
would turn a mistyped gateway on a normal subnet into a silent blackhole
instead of a park. With `mmds`, there is also a host route to
`169.254.169.254`.

**Identity.** The hostname is set. `/etc/hostname`, `/etc/hosts` (localhost,
the VM's own address, then `extra_hosts`) and `/etc/resolv.conf` are rewritten
on every boot.

**Users.** `handoff.user` and `exec.user` resolve against the new root's
`/etc/passwd` and `/etc/group`: names or numeric ids, and a numeric uid that
isn't in `/etc/passwd` is valid with `HOME=/`. A name that does not resolve
parks with `step=user`. No NSS: this is the image's files, nothing else.

**Env.** `env` arrives final. vmsetup adds only `HOME` (from `handoff.user`, or
root in supervise mode) and `TERM=linux`, each only if unset.

**Stdio.** fd 0 is `/dev/null`, fds 1 and 2 the log target.

## 6. Handoff to zpinit

vmsetup's last act is `execve`. zpinit inherits PID 1, stdio and the working
directory; nothing of vmsetup stays resident.

With `boot_timer`, vmsetup first writes byte `123` to physical `0xc0000000`
through `/dev/mem`, and Firecracker logs `Guest-boot-time = N us`: the time
from `InstanceStart` to this point, measured on the host. A failed write is a
warning, not a park; only the measurement is lost.

| Channel | Carries |
| --- | --- |
| the binary | `/run/zpinit/bin/zpinit`. In rescue before the root mounted: `/bin/zpinit` in the initramfs |
| argv | `[zpinit]` in supervise mode, `[zpinit, "--", command…]` in wrap mode, `--skip-entrypoint --config /run/zpinit/rescue.d` in rescue (§7) |
| `envp` | the composed env: zpinit's container-env tier, as under Docker |
| cwd | `handoff.working_dir` in wrap mode, `/` otherwise |
| `/run/zpinit/env` | the composed env again, `KEY=value` lines, mode 0600, for `entrypoint.d` scripts. A value with a newline or an unusual key is left out with a warning; it still arrives in `envp` |
| `/run/zpinit/machine.json` | what only the VM side knows, below |

`/run/zpinit/machine.json`, mode 0600. Its format is zpinit's; vmsetup is its
only writer:

```jsonc
{
  "v": 1,
  "console": "/dev/ttyS0",          // where the reset line goes, even with logging off
  "stop": { "signal": "SIGQUIT",    // handoff.stop_signal
            "timeout": "10s" },     // handoff.stop_timeout
  "user": "nginx",                  // handoff.user; absent in supervise mode
  "exec": {                         // absent → no listener
    "port": 1024, "user": "nginx", "working_dir": "/",
    "env": ["PATH=…"]               // the composed env, the base for every exec
  },
  "rescue": { "step": "network",    // only when parked (§7)
              "error": "…" }
}
```

zpinit enters machine mode when it is PID 1 in the kernel's initial PID
namespace, and reads `machine.json` only then. A container that ships the file
gets a warning and nothing else, which is what makes it safe for the file to
switch on a vsock listener.

## 7. Parking

When any step fails, vmsetup **parks**: the VM
stays up under zpinit's rescue mode, to be inspected over exec and then
destroyed, and no workload starts.

1. It writes the park line on `/dev/ttyS0`, a protocol line under the same
   rules as the reset line (§8):

   ```
   vmsetup: parked step=network error="interface eth0: no link with mac 06:00:DE:AD:BE:EF"
   ```

   `step` is one of `config`, `root`, `mounts`, `network`, `identity`, `user`,
   `handoff`. fctl reports parked distinctly from running and exempts it from
   its restart policy.
2. It makes sure `/run` is a tmpfs in whatever root it is in, writes
   `machine.json` with `rescue` set (and `exec` if the config was read), and
   creates the empty directory `/run/zpinit/rescue.d`.
3. It `execve`s zpinit with `--skip-entrypoint --config /run/zpinit/rescue.d`,
   so neither the image's scripts nor its services run.

If the root never mounted, rescue runs in the initramfs, with `/dev`, `/proc`
and a `/run` tmpfs. A graceful stop ends a parked VM with
`zpinit: reset intent=shutdown cause=parked`.

**If zpinit itself is missing**, there is nothing to hand the machine to.
vmsetup writes `parked step=handoff`, syncs and reboots, and with `reboot=k`
the VMM exits. It does not stay to wait for a stop, because that would be
process management. fctl sees the park line followed by the VM ending, and
treats it as a failed boot.

## 8. Ending the VM, and the reset line

This is zpinit's, stated here because fctl matches it. vmsetup never ends the
VM, except when zpinit is missing (§7).

- **Graceful stop.** fctl's `SendCtrlAltDel` reaches zpinit as `SIGINT`. In
  wrap mode zpinit sends `stop.signal` to the child and `SIGKILL`s at
  `stop.timeout`; in supervise mode it runs its ordered stop within the same
  budget. fctl's own stop timeout must be longer.
- **The host resends Ctrl+Alt+Del** while the VM is up. One sent before the
  guest's keyboard driver has finished probing, about 0.65 s after the VMM
  starts, is lost.
- **The workload ending** (the wrap-mode child exits, or whatever ends zpinit
  in a container) ends the VM. Whether it comes back is fctl's restart policy.
- **Teardown:** kill everything and reap, sync, remount `/` read-only, unmount
  the rest best effort, write the reset line, `reboot(RB_AUTOBOOT)`. With
  `reboot=k` the VMM exits.

Immediately before `reboot()`, zpinit writes one line saying why, because a
guest reset looks the same to the host whether the guest meant "I'm done",
"restart me" or panicked:

```
zpinit: reset intent=shutdown cause=exit status=0
zpinit: reset intent=shutdown cause=exit signal=SIGSEGV
zpinit: reset intent=shutdown cause=stop status=143
zpinit: reset intent=shutdown cause=parked
```

- Always its own line, preceded by `\n`, so a partial line from the workload
  cannot swallow it. fctl matches lines beginning `zpinit: reset `.
- Lines arrive as `\r\n` (the tty's `ONLCR`); strip the `\r` before matching.
- `cause=exit`: the workload ended. `cause=stop`: a graceful stop.
  `cause=parked`: a parked VM was stopped. `status=` or `signal=` is the exit
  zpinit would have reported in a container.
- Written even with `logging.target = "none"`.
- `intent=reboot` is reserved, with no producer yet, so matchers handle it
  from the start.
- No line means unknown: a panic, the guest calling `reboot(2)` itself, or a
  Ctrl+Alt+Del during vmsetup.

**Both lines are forgeable, and that is fine.** The guest owns its serial
port. Forging `shutdown` buys a stop it could get by exiting. So **this
channel must never carry anything the host acts on more strongly than that.**

## 9. Kernel and cmdline

Both are fctl's. These are what vmsetup depends on.

```
CONFIG_BLK_DEV_INITRD=y                         # the init bundle
CONFIG_VIRTIO_BLK=y  CONFIG_VIRTIO_NET=y  CONFIG_VIRTIO_MMIO=y
CONFIG_EXT4_FS=y
CONFIG_DEVTMPFS=y                               # vmsetup mounts it itself
CONFIG_DEVMEM=y                                 # the boot-timer write; STRICT_DEVMEM allows it
CONFIG_SERIAL_8250=y  CONFIG_SERIAL_8250_RUNTIME_UARTS=1   # log, park and reset lines
CONFIG_SERIO_I8042=y  CONFIG_KEYBOARD_ATKBD=y   # Ctrl+Alt+Del must reach zpinit
CONFIG_VSOCKETS=y  CONFIG_VIRTIO_VSOCKETS=y     # exec and rescue
```

The cmdline is `vmsetup_config=/dev/vda reboot=k panic=1 …`, with no root
device attached:

- **No `init=` and no `root=`.** The kernel runs the initramfs's `/init`
  unasked; `root=` would only matter if `/init` failed, and then it must panic.
- **`vmsetup_config=<device>`.** Missing is a park.
- **`reboot=k`**: `reboot()` must exit the VMM.
- **`panic=1`**: a failed `/init` or a panic resets instead of hanging.
- **No `console=` in production.** vmsetup and zpinit open `/dev/ttyS0`
  directly; on x86_64 it exists either way.
- **Never `i8042.nopnp`**: it breaks `SendCtrlAltDel`.
- **Never `ip=`**: it cannot express a `/32`.

## 10. Images and the init bundle

### 10.1 The image, and how fctl derives `handoff`

The image is an OCI filesystem as a plain ext4, with nothing of ours in it.
fctl composes the config from the image's OCI config; vmsetup never reads that
config.

- **A plain Docker image is wrap mode.** `command` is `Entrypoint + Cmd`,
  `user` is `User` (or `root`), `working_dir` is `WorkingDir` (or `/`),
  `stop_signal` is `StopSignal` (or `SIGTERM`). That is `docker run --init`.
- **A zpinit-native image** (its entrypoint is zpinit) has the bundle's zpinit
  take that entrypoint's place. With no remaining command it is supervise mode:
  no `command`, `user` or `working_dir`, and the image's `/etc/zpinit` is the
  configuration. fctl composing that configuration per launch instead is
  [#3](https://github.com/0ploy/vmsetup/issues/3).
- **Per-launch overrides.** In wrap mode, `--entrypoint`, `--user`, `-w` and a
  command apply to `handoff` as Docker would. In supervise mode they would
  belong in the service config, which vmsetup never reads, so fctl must refuse
  them rather than half-apply them. Per-launch `env` works in both modes.
- Docker's export leaves 0-byte `/etc/{resolv.conf,hosts,hostname}` and a fake
  `/dev/console` regular file; vmsetup copes with both. Removing `/.dockerenv`
  is the image builder's job.

### 10.2 The init bundle

Built by `cmd/mkbundle`, attached by fctl as `initrd_path` on every boot. An
**uncompressed newc cpio**: Firecracker copies it into guest memory as-is, and
decompressing it would be boot time spent for nothing.

```
/init                   vmsetup, static, CGO_ENABLED=0
/bin/zpinit             zpinit, with machine mode
/bin/zpctl
/dev/console            c 5 1 — the kernel opens it for /init's stdio
/dev/  /proc/  /newroot/  /run/   empty mount points
```

- **Reproducible.** Sorted entries, mtime 0, uid/gid 0, inodes by position,
  nothing from the host but file contents. The same binaries give a
  byte-identical cpio, so fctl can content-address it.
- **Its unpacked size is guest RAM** until vmsetup frees it: about 9 MiB.
- An image's own zpinit is never what runs, so zpinit's config format must
  stay backwards compatible, which as a shipping product it promises anyway.

## 11. Security

The workload is untrusted and controls its own disk and guest kernel state.

- **The VM boundary is the only boundary.** vmsetup enforces nothing.
- **The workload owns its disk but not PID 1.** Anything on the writable root
  could be overwritten and would own the next boot, and with it the stop, the
  reset line, rescue and exec, which the host relies on. So vmsetup and zpinit
  come from the bundle on every boot, and zpinit runs from a read-only tmpfs
  copy, never from the disk.
- **The boot fails closed** (§3).
- **Freeing the initramfs refuses to run on a disk root** (§5).
- **exec exists only in machine mode and accepts only the host CID**
  (zpinit#10).
- **The serial lines are forgeable by design** (§8).
- **MMDS is guest-readable by definition** and must never carry host secrets.
