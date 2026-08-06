# vminit — implementation plan

Target: a dedicated implementation session on a Linux host with KVM.

This document is the spec. Background and evidence for the decisions live in
[docs/RESEARCH.md](docs/RESEARCH.md); read that first if a choice here looks
arbitrary.

---

## 1. Scope

**In scope**

- `vminit boot` — PID 1 inside the microVM: machine bringup, then handoff
- `vminit agent` — post-boot MMDS poller that applies live config changes
- `vmpack` — host-side tool converting an OCI image into a bootable ext4 rootfs
- The config schema shared by all three

**Out of scope**

- Process supervision — that is [zpinit](https://github.com/0ploy/zpinit)'s job
  (see `ZPINIT_CHANGES.md` in the firecracker research workspace for the
  changes zpinit needs)
- VM lifecycle, TAP devices, host firewall — that is
  `firecracker-manager` (fcm)
- Snapshots, live migration, aarch64. Deliberately deferred; see §12.

**Target platform:** x86_64 only for now. Linux guest kernel 5.10+.

---

## 2. Design principles

1. **vminit is not a supervisor.** It brings the machine up and `execve`s
   something else. After the exec it does not exist. This keeps it small,
   makes its runtime footprint irrelevant, and means a bug in supervision is
   not a bug in vminit.
2. **Supervisor-agnostic.** The handoff target is configuration. zpinit is the
   default, not a requirement.
3. **No dependency on the guest image.** Everything is done via direct
   syscalls from a static binary. vminit never shells out to `ip`, `mount`,
   `hostname`, or anything else, because none of those are guaranteed to
   exist — `scratch` images have no userspace at all.
4. **Enforce on the host, never in the guest.** The workload is untrusted and
   owns its own rootfs; it can replace `/sbin/vminit` if it wants. That is
   fine, because the VM boundary is the security boundary. All limits — vCPU,
   memory, I/O, network — are enforced by Firecracker and the host. vminit
   enforces nothing.
5. **Fail loudly at boot, never silently.** A misconfigured network that boots
   anyway is worse than a VM that refuses to start with a clear message.

---

## 3. Boot sequence

```
Firecracker
  └─ kernel (vmlinux, init=/sbin/vminit)
       └─ vminit boot                      ← PID 1
            1. read build-time config
            2. merge boot-time override, if present
            3. mount filesystems
            4. configure network via netlink
            5. hostname, /etc/hosts, /etc/resolv.conf
            6. resolve user, compose env
            7. write /run/zpinit/env (handoff=zpinit only)
            8. execve(handoff target)      ← vminit ends here
                 └─ zpinit                 ← becomes PID 1
                      ├─ vminit agent      ← supervised as a normal service
                      └─ the workload
```

The agent is a **service supervised by zpinit**, not part of vminit's PID 1
path. vminit has exec'd away by then; something still alive has to poll MMDS.

---

## 4. Configuration

### 4.1 Layering

Three sources, merged in order, each overriding the last:

| Layer | Location | Written by | When |
| --- | --- | --- | --- |
| Build-time defaults | `/etc/vminit/config.json` | `vmpack` | image build |
| Boot-time override | `/etc/vminit/override.json` | fcm | before each boot |
| Live updates | MMDS | control plane | while running |

Build-time defaults come from the OCI image config. The boot-time override is
the `docker run --entrypoint / -e / --user` equivalent — it lets fcm change
per-launch settings without rebuilding the rootfs. MMDS handles what must
change on a running VM.

Merge is **shallow per top-level key**, except `env`, which merges per
variable. `exec_override` replaces `entrypoint` + `cmd` wholesale — matching
fly's model (`fly-io-init/src/lib.rs:16-33`) and Docker's semantics.

### 4.2 Schema

```jsonc
{
  "hostname": "web-01",

  // Straight from the OCI image config. vmpack fills these in.
  "image": {
    "entrypoint":  ["/docker-entrypoint.sh"],
    "cmd":         ["nginx", "-g", "daemon off;"],
    "env":         ["PATH=/usr/local/sbin:...", "NGINX_VERSION=1.31.3"],
    "working_dir": "/",
    "user":        "root",
    "stop_signal": "SIGQUIT"
  },

  // Per-launch overrides. Any may be absent.
  "exec_override": null,
  "user_override": null,
  "extra_env":     { "DEPLOY_ENV": "prod" },

  "network": {
    "interfaces": [
      {
        "name":    "eth0",
        "mac":     "06:00:AC:10:00:02",   // optional; pin if multiple NICs
        "address": "172.16.0.2/30",
        "gateway": "172.16.0.1",
        "mmds":    true                    // add route to the MMDS IP
      }
    ],
    "nameservers":  ["1.1.1.1", "8.8.8.8"],
    "search":       [],
    "extra_hosts":  [{ "ip": "10.0.0.5", "host": "db.internal" }]
  },

  "mounts": [
    { "device": "/dev/vdc", "path": "/data", "fstype": "ext4", "options": "relatime" }
  ],

  "handoff": {
    "mode":   "zpinit",          // "zpinit" | "exec" | "custom"
    "path":   "/usr/local/bin/zpinit",
    "args":   ["--vm"]
  },

  "logging": {
    "target": "serial"           // "serial" | "none"
  },

  "mmds": {
    "enabled": true,
    "address": "169.254.169.254",
    "version": "V2",
    "poll_interval": "30s"
  }
}
```

### 4.3 Handoff modes

- **`zpinit`** — `execve("/usr/local/bin/zpinit", ["--vm"])`. Composed env is
  passed via `/run/zpinit/env` (zpinit's existing format) rather than the
  process environment, so `entrypoint.d` scripts can extend it. The main
  service TOML was generated by `vmpack` at build time.
- **`exec`** — `execve` the resolved entrypoint+cmd directly, with the composed
  env, as the resolved user, in `working_dir`. **Warn loudly**: the workload
  becomes PID 1, so nothing reaps orphans and its exit will panic the kernel
  unless it handles `reboot()` itself. Intended for single-process images that
  are known to behave, and for debugging.
- **`custom`** — `execve` an arbitrary `path` + `args`. Same caveats as `exec`.

---

## 5. `vminit boot`

Ordered steps. Each must be idempotent where possible and log its failure
clearly to the console before aborting.

### 5.1 Read config

Open `/etc/vminit/config.json`; merge `/etc/vminit/override.json` if present.
A missing base config is fatal. A malformed override is fatal — do not
silently fall back, since it likely carries the network config.

### 5.2 Mount filesystems

Ordered, mirroring `fly-io-init/src/bin/init/main.rs:187-330`:

| Mount | Type | Flags |
| --- | --- | --- |
| `/dev` | devtmpfs | `MS_NOSUID`, `mode=0755` |
| `/proc` | proc | `MS_NODEV\|MS_NOEXEC\|MS_NOSUID` |
| `/sys` | sysfs | same |
| `/dev/pts` | devpts | `MS_NOEXEC\|MS_NOSUID\|MS_NOATIME`, `mode=0620,gid=5,ptmxmode=666` |
| `/dev/shm` | tmpfs | `MS_NOSUID\|MS_NODEV`, mode 1777 |
| `/dev/mqueue` | mqueue | common flags |
| `/run` | tmpfs | `MS_NOSUID\|MS_NODEV`, `mode=0755` |
| `/sys/fs/cgroup` | cgroup2 | `MS_NODEV\|MS_NOEXEC\|MS_NOSUID\|MS_RELATIME` |

Then the symlinks: `/dev/fd → /proc/self/fd`, `/dev/stdin → /proc/self/fd/0`,
`/dev/stdout → /proc/self/fd/1`, `/dev/stderr → /proc/self/fd/2`.

`mkdir` failures with `EEXIST` are not errors. Mount **cgroup2 unified only** —
do not replicate fly's cgroup v1 hierarchy; it exists for legacy workloads and
modern images do not need it.

Then apply `config.mounts` for any extra data volumes.

> Docker's export leaves `/etc/resolv.conf`, `/etc/hosts`, `/etc/hostname` as
> **0-byte regular files** and `/dev/console` as a **0-byte regular file, not a
> device node** — even for `scratch`. Mounting devtmpfs over `/dev` shadows the
> fake console. The three `/etc` files must be written in §5.4.

### 5.3 Network via netlink

Use `github.com/vishvananda/netlink` — pure Go, no cgo. **Never shell out to
`ip`**; `scratch` has no iproute2 and Alpine's busybox `ip` differs from
Debian's.

For each configured interface:

1. Find the link by name, or by MAC if `mac` is set. Firecracker does not
   guarantee NIC enumeration order matches API call order, so pin by MAC
   whenever there is more than one interface.
2. `LinkSetUp`
3. `AddrAdd` the address
4. `RouteAdd` the default route via `gateway`
5. If `mmds: true`, `RouteAdd` a host route to the MMDS address on this link

Always bring `lo` up. Failing to do so breaks a surprising amount of software.

**One NIC is sufficient for MMDS.** Firecracker's own documentation attaches
MMDS to `eth0`; a dedicated MMDS interface is unnecessary. fcm currently
creates a second TAP (`mmds0`) — that should be removed (§10).

### 5.4 Identity files

- `sethostname(config.hostname)` and write `/etc/hostname`
- Write `/etc/hosts`: `127.0.0.1 localhost`, `::1 localhost`, the VM's own
  address → hostname, then `network.extra_hosts`
- Write `/etc/resolv.conf` from `network.nameservers` and `search`

Truncate and rewrite rather than append — the files exist but are empty, and
re-running must be idempotent.

### 5.5 Resolve user and compose env

Resolve `user_override` (else `image.user`, else `root`) in `user[:group]`
form. Accept both names and raw numeric ids. Parse `/etc/passwd` and
`/etc/group` from the guest rootfs directly.

> Build with `CGO_ENABLED=0`. Go's `os/user` then uses its pure-Go
> implementation, which parses `/etc/passwd` directly — exactly what is wanted.
> It will not honor NSS (LDAP etc.), which is correct for a container image.

Env precedence, lowest to highest: `image.env` → `extra_env` → MMDS-supplied
env. Then:

- if `PATH` is set, apply it to vminit's own environment so `argv[0]` lookup works
- default `HOME` to the resolved user's home directory if not already set
- default `TERM=linux` if not set

### 5.6 Handoff

Per §4.3. For `handoff.mode = "zpinit"`, write the composed env to
`/run/zpinit/env` in zpinit's `key=value` format first — `/run` is a tmpfs
mounted in §5.2, so this is cheap and disappears on shutdown.

`execve` — never `fork`. vminit must not linger.

---

## 6. `vminit agent`

A long-running process supervised by zpinit as an ordinary service. Ships in
the same binary; `vmpack` generates its service TOML.

Loop, every `mmds.poll_interval`:

1. Fetch the MMDS document. **Use V2** — acquire a token with a
   `PUT /latest/api/token` carrying `X-metadata-token-ttl-seconds`, then `GET`
   with `X-metadata-token`. V1 is deprecated and unauthenticated.
2. Diff against the last-applied state.
3. Apply what changed, and only what changed:
   - `network.*` → re-apply via netlink (idempotent: add-if-absent, remove stale)
   - `nameservers` / `extra_hosts` → rewrite `/etc/resolv.conf`, `/etc/hosts`
   - `hostname` → `sethostname` + rewrite
   - `env` → update `/run/zpinit/env`, then `zpctl reload` the affected
     services so they respawn with it
4. Persist the merged result to `/etc/vminit/config.json` so a reboot keeps it.

**MMDS is guest-readable by definition.** Anything placed there is visible to
the workload. Never put host-side secrets in it that the tenant should not see.

Failure policy: log and retry with backoff. The agent must never take the VM
down — a stale config is better than a dead machine.

---

## 7. Kernel and boot args

### 7.1 Guest kernel

Start from Firecracker's reference config
(`resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config`). Verified
present and required:

```
CONFIG_IP_PNP=y                 # kernel-level ip= fallback
CONFIG_VIRTIO_BLK=y  CONFIG_VIRTIO_NET=y  CONFIG_VIRTIO_MMIO=y
CONFIG_EXT4_FS=y  CONFIG_DEVTMPFS=y
CONFIG_VSOCKETS=y  CONFIG_VIRTIO_VSOCKETS=y     # for later host-side exec
CONFIG_SERIO_I8042=y  CONFIG_KEYBOARD_ATKBD=y   # required for SendCtrlAltDel
CONFIG_SERIAL_8250=y  CONFIG_SERIAL_8250_RUNTIME_UARTS=1
```

Use uncompressed **`vmlinux`**, not `bzImage` — decompression costs boot time
and guest memory.

### 7.2 Boot args

```sh
# production
init=/sbin/vminit quiet reboot=k panic=1 pci=off nomodules \
  i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd

# debug
init=/sbin/vminit console=ttyS0 loglevel=7 reboot=k panic=1 pci=off
```

Console handling must be a **runtime flag on the host side**, not a build-time
property of the image.

Omitting `console=` stops the *kernel* logging to the emulated UART, which is
the expensive part — every byte is a synchronous VM exit and the boot log is
hundreds of lines. It does **not** remove the device: on x86_64 Firecracker
creates COM1 unconditionally, and the kernel still enumerates `/dev/ttyS0`.
So userspace logging keeps working with no console spam. See
docs/RESEARCH.md §4 — **verify this empirically first**, the Firecracker
swagger doc contradicts the code.

Keep `i8042` itself enabled; the `i8042.*` flags only skip probing. Disabling
the driver would break `SendCtrlAltDel`, which is the only graceful shutdown
channel on x86.

**Optionally also pass `ip=`** as a belt-and-braces fallback so the VM has
connectivity even if vminit dies before §5.3:

```
ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off
```

---

## 8. `vmpack` — OCI image → bootable rootfs

Host-side. Runs on Linux (needs loop mounts / `mkfs.ext4 -d`).

1. **Pull** via `skopeo` or `crane` — **not** the Docker daemon. Resolve the
   OCI index to the `linux/amd64` manifest explicitly.
   > On Docker 29 with the containerd image store, `docker image inspect` on a
   > cross-platform image returns `"Config": {}` with no error, and
   > `docker save` fails outright. Verified. Do not build on the daemon.
2. **Read** `config.json` from the config blob: `Entrypoint`, `Cmd`, `Env`,
   `WorkingDir`, `User`, `StopSignal`.
3. **Extract** layers in order, applying `.wh.` whiteouts and `.wh..wh..opq`
   opaque-directory markers. Preserve symlinks, hardlinks, modes, xattrs.
4. **Clean** Docker's synthetic artifacts: remove `/.dockerenv`; truncate the
   0-byte `/etc/{resolv.conf,hosts,hostname}`; remove the fake `/dev/console`
   regular file.
5. **Inject**:
   - `/sbin/vminit` (static, `CGO_ENABLED=0`)
   - `/usr/local/bin/zpinit` + `zpctl`, if `handoff.mode = "zpinit"`
   - `/etc/vminit/config.json` generated from the image config
   - `/etc/zpinit/services/10_main.toml` translating entrypoint+cmd+user+workdir
   - `/etc/zpinit/services/00_vminit_agent.toml` for the MMDS agent
6. **Size and format**: compute the extracted tree size, add configurable
   slack, `truncate` + `mkfs.ext4 -d <tree>`. Building from a directory avoids
   needing a loop mount and works unprivileged in most setups.
7. **Emit** a manifest describing the produced artifact: rootfs path, size,
   the config that was baked in, and the recommended boot args.

`vmpack` must be deterministic — same image digest plus same vminit version
produces a byte-identical rootfs. That enables content-addressed dedup later.

---

## 9. Security model

The workload is untrusted and has full control of its own rootfs and guest
kernel state. Therefore:

- **The VM boundary is the only boundary.** A tenant replacing `/sbin/vminit`
  is expected and harmless.
- **All limits are host-enforced**: vCPU and memory by Firecracker, disk and
  network throughput by Firecracker's rate limiters, traffic policy by
  nftables on the TAP, process isolation by the **jailer** plus seccomp.
- Run Firecracker under the **jailer** — see the upstream
  `docs/prod-host-setup.md`.
- **MMDS V2 only.** V1 is unauthenticated; any process in the guest, including
  one reached via SSRF in the tenant's own app, can read it.
- Firecracker rate-limits serial output via a `TokenBucket`, so a guest cannot
  flood the host through the log channel. Confirm it is configured.

---

## 10. Host-side changes in `firecracker-manager`

| Change | Where | Why |
| --- | --- | --- |
| Drop the `mmds0` TAP; attach MMDS to `eth0` | `lib/vm_lifecycle.py:113-135`, `lib/network_manager.py:144` | One NIC suffices; removes a device to create, track and leak |
| Make boot args configurable, with prod/debug profiles | `lib/firecracker_api.py:65` | Currently hardcoded `console=ttyS0 reboot=k panic=1 pci=off` |
| Set `serial_out_path` via `PUT /serial` | `lib/firecracker_api.py` | Per-VM log file on the host, no vsock needed |
| Enforce MMDS `version: "V2"` | `lib/firecracker_api.py:109` | Currently defaults to deprecated V1 |
| Write `/etc/vminit/override.json` before boot | `lib/filesystem_manager.py` | Per-launch config without rebuilding the rootfs |
| Graceful stop via `SendCtrlAltDel`, not SIGKILL | `lib/vm_lifecycle.py` | Lets the guest flush and unmount |
| Run under jailer | `lib/vm_lifecycle.py` | Untrusted workloads |

---

## 11. Testing

Most of this cannot be tested on macOS. Plan for a Linux host with KVM.

- **Unit** — config merge precedence, OCI→zpinit TOML translation, env
  composition order, user resolution incl. numeric ids and missing `/etc/passwd`.
- **`vmpack` golden tests** — pack `alpine`, `debian`, `nginx:alpine` and a
  `scratch` image; assert tree contents, injected files, and determinism
  (pack twice, compare hashes).
- **Boot matrix** — the same four images must boot, get an IP, resolve DNS,
  and run their entrypoint. `scratch` is the important one: it proves zero
  guest-userspace dependency.
- **Shutdown** — `SendCtrlAltDel` results in a clean stop and the Firecracker
  process exits; assert no `Attempted to kill init` panic.
- **Boot time** — measure with and without `console=`. Record the numbers; the
  console claim in §7.2 is load-bearing and currently unverified.
- **Agent** — `PATCH /mmds` while running, assert the change is applied and
  survives a reboot.

---

## 12. Deferred

Not now, but nothing here should be designed against:

- **aarch64** — no i8042, so shutdown needs a different trigger; the serial
  device is MMIO and *is* conditional on `console=`
- **Snapshots** — restoring N VMs from one snapshot brings duplicate machine
  ids, entropy pools and clocks, vsock UDS collisions, and expected packet loss
- **Passthrough mode** for images with their own init (systemd/OpenRC), where
  vminit must *not* run and the kernel `ip=` parameter does the networking.
  Note systemd-networkd may clobber a kernel-configured interface.
- **virtio-pmem rootfs** — DAX-mapped, shared host page cache; faster and more
  memory-efficient than virtio-blk for read-only roots. Needs extra kernel config.
- **squashfs + overlay** rootfs for CoW and cross-VM dedup
- **Host-side `exec`** — needs `zpctl exec` and a vsock listener; see
  `ZPINIT_CHANGES.md` §9 and §6

---

## 13. Milestones

**M1 — `vmpack` produces a bootable rootfs.**
Pull, extract, clean, size, format. Inject a hand-written config. Boot it with
a stub vminit that only mounts and execs `/bin/sh`. Proves the pipeline.

**M2 — `vminit boot` full bringup.**
Mounts, netlink, identity files, user resolution, env composition, handoff in
`exec` mode. Success = `nginx:alpine` boots, serves traffic, resolves DNS.

**M3 — zpinit handoff.**
`handoff.mode = "zpinit"`, generated service TOML, `/run/zpinit/env`. Requires
zpinit sections 1, 2, 4 and 5 from `ZPINIT_CHANGES.md`. Success = a
multi-process image with restarts and readiness probes.

**M4 — `scratch` and the boot matrix.**
All four test images. This is the real proof of distro independence.

**M5 — `vminit agent` and MMDS.**
Live reconfiguration, V2 tokens, persistence across reboot.

**M6 — production hardening.**
Jailer, rate limiters, console-off boot args, `serial_out_path`, fcm changes
from §10.

M1 and M2 are the ones that retire the most risk. Everything after is additive.

---

## 14. Open questions

1. **Does `/dev/ttyS0` work with `console=` omitted on x86_64?** The code says
   yes, the swagger doc says no. Settle this on real hardware before §7.2 is
   built on. If the doc turns out to be right, the log path becomes vsock and
   `ZPINIT_CHANGES.md` §7 needs revisiting.
2. **Rootfs sizing policy** — fixed slack, percentage, or caller-specified?
   Interacts with whether `/data` is a separate volume.
3. **Does `vmpack` belong in this repo long-term?** Argument for: it writes
   the schema `vminit` reads, and drift between them is the likeliest bug.
   Argument against: it is host-side and may end up inside fcm.
4. **Agent poll interval, or push?** Polling MMDS every 30s is simple but
   laggy. A vsock nudge from the host would be immediate — but that is the
   same vsock work as host-side exec, so it may come for free later.
