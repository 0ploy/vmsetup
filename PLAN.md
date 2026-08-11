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
- VM lifecycle, TAP devices, host firewall, the kernel command line, guest
  kernel builds, snapshots — that is [fctl](../fctl), the host-side VM
  manager. Its research notes are the companion to this document
- Live migration and aarch64. Deliberately deferred; see §12.

**Target platform:** x86_64 only for now. Linux guest kernel 5.10+.

---

## 2. Design principles

1. **vminit is not a supervisor.** It brings the machine up and hands off.
   In the normal case it `execve`s and ceases to exist, which keeps it small
   and makes its runtime footprint irrelevant. The one exception is
   `profile = "command"` (§4.3), where it stays as PID 1 to reap and
   own the machine lifecycle for a single unsupervised process — but even
   there it runs one child and does not restart, probe, or schedule. Anything
   resembling real supervision belongs to zpinit.
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
6. **Security and isolation come first.** Performance is taken only where it is
   harmless to isolation. The workload is untrusted and the VM boundary is the
   only boundary, so a density or latency win that weakens it is not a win —
   and vminit sits on the guest side of that boundary, so anything it does with
   shared state is load-bearing. The host-side reasoning and the rejected
   options are in [fctl/docs/threat-model.md](../fctl/docs/threat-model.md);
   the guest-side consequences are §5.2 (overlay root) and §12 (VMGenID).

---

## 3. Boot sequence

```
Firecracker
  └─ kernel (vmlinux, init=/sbin/vminit)
       └─ vminit boot                      ← PID 1
            1. read build-time config
            2. merge boot-time override, if present
            3. assemble overlay root + pivot_root, if configured
            4. mount filesystems
            5. configure network via netlink
            6. hostname, /etc/hosts, /etc/resolv.conf
            7. resolve user, compose env
            8. write /run/zpinit/env (profile=zpinit only)
            9. execve(profile target)      ← vminit ends here
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
| Boot-time override | `/etc/vminit/override.json` | fctl | before each boot |
| Live updates | MMDS | control plane | while running |

Build-time defaults come from the OCI image config. The boot-time override is
the `docker run --entrypoint / -e / --user` equivalent — it lets fctl change
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

  // Optional. When present, vminit assembles an overlay root before anything
  // else and pivot_roots into it — see §5.2. lower is the shared read-only
  // image, upper is this VM's private writable layer. fctl attaches both
  // drives and fills this in; vminit never chooses the layout.
  "overlay_root": {
    "lower":  { "device": "/dev/vda", "fstype": "squashfs" },
    "upper":  { "device": "/dev/vdb", "fstype": "ext4" },   // or "tmpfs"
    "workdir": "/overlay/work"
  },

  // One flat axis — see §4.3. Decides both what vminit does to the machine
  // and whether it survives the handoff.
  "boot": {
    // zpinit | command | cloud-init | systemd | openrc
    "profile": "zpinit",

    // profile = "zpinit" only
    "path": "/usr/local/bin/zpinit",
    "args": ["--vm"],

    // profile = "systemd" only — openrc and cloud-init are fully determined
    "network": "auto",   // auto | netplan | systemd-networkd | ifupdown

    // generate profiles only; override when the image's init is elsewhere
    "init": "/sbin/init"
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

### 4.3 Profiles — who configures the network

One `profile` value decides everything. Read it as an answer to a single
question: **who is going to configure this machine's network?**

| `profile` | configured by | vminit's job | handoff |
| --- | --- | --- | --- |
| `zpinit` | vminit | apply (§5) | `execve` zpinit |
| `command` | vminit | apply (§5) | fork + supervise |
| `cloud-init` | cloud-init | pin the datasource | `execve` init |
| `systemd` | systemd's network stack | write config + enable it | `execve` init |
| `openrc` | OpenRC's `networking` service | write config + enable it | `execve` init |

The handoff column follows from one rule: **`execve` only when the target
requires PID 1; otherwise fork.** zpinit requires it (its reaper is written for
it). systemd requires it (checks `getpid() == 1` and refuses to run as a system
manager otherwise). A bare command does not.

```toml
[boot]
profile = "zpinit"

# read only when profile = "systemd" — see §4.4
network = "auto"     # auto | netplan | systemd-networkd | ifupdown

init = "/sbin/init"  # generate profiles only; override if elsewhere
```

- **`zpinit`** — zpinit becomes PID 1, which is what
  `internal/reaper/reaper.go` is written for: "As PID 1 zpinit must reap every
  child, including orphans reparented to us … after a double-fork." It does
  **not** set `PR_SET_CHILD_SUBREAPER`. Composed env goes via
  `/run/zpinit/env`, so `entrypoint.d` scripts can extend it.
- **`command`** — the default for single-process images. vminit stays PID 1
  and forks the workload, which is strictly better than exec'ing it:
  - PID 1 does not get default signal dispositions, so a workload exec'd as
    PID 1 with no `SIGTERM` handler **silently ignores SIGTERM**. Forked, it
    gets normal semantics with vminit forwarding.
  - vminit can capture the exit status and report it (§7) before rebooting.
    Exec'd, nobody can — the VM simply disappears.
  - orphans get reaped; shutdown does not require the workload to call
    `reboot()` itself.

  There is deliberately no exec-a-bare-command profile. It loses on every axis
  above and gains one saved process.
- **`cloud-init`, `systemd`, `openrc`** — generate profiles, for full-distro
  images you build yourself. See §4.4.

### 4.4 Generate profiles — for images you build yourself

These are **not** for registry images (§4.6). They are for images you build:
start from `ubuntu`, `apt install systemd`, add your software, ship a full VM.

systemd and OpenRC own mounts, cgroups and network management. vminit must
therefore **mount nothing** and configure nothing imperatively — it writes
files the running system will consume, then execs.

#### Why the profile names the init system, not the config format

Two things vary, and they are only partly correlated:

- **the init system** — determines how a service gets *enabled*, and what to exec
- **the network config format** — determines which file gets *written*

Naming by format alone breaks, because `ifupdown` spans both init systems:

| | Debian | Alpine |
| --- | --- | --- |
| package | `ifupdown` 0.8.41 | `ifupdown-ng` 0.12.1 |
| config file | `/etc/network/interfaces` | `/etc/network/interfaces` |
| managed by | `networking.service` (**systemd** unit) | `/etc/init.d/networking` (**OpenRC**) |
| enabled by | `multi-user.target.wants/` symlink | `/etc/runlevels/boot/` symlink |

Same file, entirely different enablement. Naming by distro breaks too: Ubuntu
server uses netplan, Debian with systemd defaults to ifupdown, Debian cloud
images use networkd — so a `debian` profile would be ambiguous.

The asymmetry that resolves it: **`openrc` fully determines everything**
(Alpine → ifupdown-ng → runlevel symlink, one combination), while **`systemd`
leaves the format open**. Hence one sub-option, `network`, read only under
`profile = "systemd"`.

#### What each profile writes

| profile | `network` | writes | then enables |
| --- | --- | --- | --- |
| `cloud-init` | — | `/etc/cloud/cloud.cfg.d/90-vminit.cfg` datasource pin | nothing; MMDS in EC2 format does the rest |
| `systemd` | `netplan` | `/etc/netplan/50-vminit.yaml`, **mode 0600** | nothing; netplan renders to networkd at boot |
| `systemd` | `systemd-networkd` | `/etc/systemd/network/10-<iface>.network` | symlink `systemd-networkd.service` into `multi-user.target.wants/` — **Debian ships it disabled** |
| `systemd` | `ifupdown` | `/etc/network/interfaces` | symlink `networking.service` into `multi-user.target.wants/` |
| `openrc` | — | `/etc/network/interfaces` (conservative ifupdown-ng syntax) | symlink `/etc/init.d/networking` into `/etc/runlevels/boot/`, likewise `hostname` |

The easy-to-miss part is the last column. **Writing the file is not enough**
when the consuming service is installed but not enabled, and vminit cannot run
`systemctl enable` or `rc-update add` because neither init is running yet — it
must create the symlink directly.

`network = "auto"` detects in order: `/usr/sbin/netplan` → `netplan`;
`/etc/network/interfaces` present → `ifupdown`; else `systemd-networkd`.
Always overridable; detection is a convenience, not a contract.

Write the conservative common subset of `/etc/network/interfaces` syntax —
Debian's `ifupdown` and Alpine's `ifupdown-ng` are different implementations
and only broadly compatible. Plain `iface eth0 inet static` with
`address`/`gateway` works on both.

DNS splits differently again and does not follow the same lines:

- with systemd-resolved enabled, `/etc/resolv.conf` is a symlink to a stub and
  direct writes are overwritten — nameservers must go in `DNS=` in the
  `.network` file or netplan YAML
- without resolved (Alpine, minimal Debian), write `/etc/resolv.conf` directly

### 4.5 If you are building the image, just install cloud-init

Since these images are self-built, there is a shortcut that removes most of
§4.4: **install cloud-init and use `profile = "cloud-init"`.** Packages exist
for Alpine as well as Debian and Ubuntu.

MMDS's native schema is already EC2-shaped — that is why Firecracker's own
examples use `ami-id`, `reservation-id` and `local-hostname` under
`latest/meta-data`. cloud-init's Ec2 datasource consumes exactly that layout:

```
boot /sbin/init  +  MMDS populated in EC2 format  =  configured VM
```

One mechanism covers every full-distro image regardless of base or init
system, and it sidesteps the entire enablement problem above. Build `systemd`
and `openrc` only when a specific image cannot take cloud-init.

The one file this profile still writes: pin the datasource in
`/etc/cloud/cloud.cfg.d/90-vminit.cfg` with `datasource_list: [ Ec2, None ]`.
Without it cloud-init probes every datasource it knows and can add seconds to
boot.

### 4.6 Registry images never use the generate profiles

Verified by running `debian:12`, `ubuntu:24.04` and `alpine:3.20`: **none
contains an init system at all** — no systemd, no OpenRC, no netplan, no
cloud-init. Container base images strip them.

So the generate profiles apply only to images built for this purpose. The two
paths have disjoint image sources, and `vmpack` should treat "init present in
an OCI image" as the rare exception it is (`ubi9/ubi-init`, `jrei/systemd-ubuntu`
and similar) rather than something to probe for on every build.

| Image source | `profile` |
| --- | --- |
| OCI, multi-process | `zpinit` |
| OCI, single process | `command` |
| Self-built full distro, with cloud-init | `cloud-init` |
| Self-built full distro, without | `systemd` or `openrc` |

The first two are the product. Set expectations for the rest: a full distro
boots in **seconds, not milliseconds**, wants a writable rootfs, and
realistically needs 256 MB or more. Firecracker's fast-boot property does not
survive systemd. That is inherent, not fixable here.

---

## 5. `vminit boot`

Ordered steps. Each must be idempotent where possible and log its failure
clearly to the console before aborting.

> **Implemented ordering differs in one place, deliberately.** devtmpfs is
> mounted on `/dev` *before* the config is read, not after. "Log its failure
> clearly to the console" is not achievable otherwise: a rootfs exported from
> an OCI image contains no device nodes, so until devtmpfs is up there is no
> `/dev/ttyS0` to log to, and reading the config is the step most likely to
> fail. Nothing about mounting `/dev` depends on the config, so the reordering
> costs nothing. Everything logged before the port opens is buffered and
> flushed the moment it does. [VERIFIED] — a truncated `override.json` reports
> `override config is not a JSON object` on the host's serial log and the VM
> reboots without a kernel panic.
>
> The consequence for §5.2's overlay root, whenever it is built: `/dev` is
> already mounted by then, so it must be `MS_MOVE`d into the merged root
> rather than mounted again, the way
> `fly-io-init/src/bin/init/main.rs:214` does it.

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

#### `EBUSY` on `/dev` is the normal case, not an edge case

The reference guest kernel sets `CONFIG_DEVTMPFS_MOUNT=y`, so **the kernel has
already mounted devtmpfs on `/dev` before it execs `init=`** — and it does so
with none of the flags in the table above. Mounting it again returns `EBUSY`.

Treating that as "already done, carry on" silently loses `nosuid` on `/dev`.
[VERIFIED] — the boot test caught exactly this. So `EBUSY` means adopt the
existing mount and apply the flags to it with
`MS_REMOUNT|MS_BIND`, which changes the per-mount flags without touching the
superblock. The distinction matters for devtmpfs specifically, which is one
kernel-wide instance rather than one filesystem per mount.

#### Remounting `/` read-write

The kernel mounts the root filesystem read-only unless `rw` is on the command
line, and everything in §5.4 writes to `/etc`. vminit therefore checks and
remounts if needed, which is belt-and-braces rather than load-bearing on
Firecracker: it **appends `root=/dev/vda rw` to the boot args by itself**
[VERIFIED], so `/` arrives writable even when the caller's `boot_args` say
nothing about it. Failing to remount is a warning, not an abort — the
subsequent write failure names the file it could not write, which is a more
useful message than one about mount flags.

#### Overlay root, when `overlay_root` is set

This runs **first**, before the table above, because everything else mounts
into the merged root. Mount the lower read-only, mount the upper, then the
overlay, then `pivot_root` into it:

```
mount(lower.device, /overlay/lower, lower.fstype, MS_RDONLY, "")
mount(upper.device, /overlay/upper, upper.fstype, 0, "")
mount("overlay", /overlay/merged, "overlay", 0,
      "lowerdir=/overlay/lower,upperdir=/overlay/upper/root,workdir=...")
pivot_root(/overlay/merged, ...)
```

This is the shared-squashfs + per-VM-overlay design from
[Firecracker discussion #3061][3061] and
[fctl/docs/performance.md](../fctl/docs/performance.md). It is the main density
lever available, and it is safe because the shared layer is read-only,
identity-addressed, and never mapped into the guest — writes land in this VM's
private upper layer by construction.

**vminit does this itself rather than shipping upstream's `overlay-init`.**
That is a shell script used as `init=`, which contradicts design principle 3
(`scratch` images have no shell) and carries a reported failure mode where a
non-executable script fails the boot with a permission error. A static PID 1
that already owns mounting has neither problem.

The lower layer must be mounted `MS_RDONLY` and vminit must **fail loudly** if
it cannot be — a writable shared lower layer is a cross-tenant write channel,
not a degraded mode to continue in (principle 6).

[3061]: https://github.com/firecracker-microvm/firecracker/discussions/3061

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
4. `RouteAdd` the default route via `gateway`, **with `RTNH_F_ONLINK` when the
   address does not cover the gateway** — see below
5. If `mmds: true`, `RouteAdd` a host route to the MMDS address on this link

#### The default route needs `onlink` under a /32 **[VERIFIED]**

fctl allocates each VM **one routed address with no on-link subnet at all** —
a bare `/32` whose gateway is a link-local address that every tap on the host
carries ([fctl RESEARCH.md §7](../fctl/docs/RESEARCH.md)). One address per VM
instead of the four a `/30` burns, and the gateway is then identical for every
VM everywhere, so the guest's configuration no longer depends on which address
it was allocated.

The consequence here is small but absolute: with a `/32` the gateway is inside
no connected prefix, so a plain route add fails and the VM boots with no
default route. Verified, by disabling the flag and re-running the boot test:

```
[vminit   11.992ms] configured eth0 with 10.77.0.5/32
[vminit   12.659ms] error: bringup failed: interface eth0: add default route
                    via 169.254.0.1 on eth0: network is unreachable
```

`RTNH_F_ONLINK` tells the kernel to treat the nexthop as directly reachable,
which it genuinely is — the tap is point-to-point with the host at the other
end.

**The flag is computed, not configured.** `netconf.gatewayNeedsOnlink` sets it
only when the interface's own address really does not contain the gateway.
Two reasons for deriving it rather than adding a schema field:

- a field would be one more thing fctl and vminit can disagree about, and
  drift between the writer and the reader of this schema is the likeliest bug
  in the whole design;
- setting `onlink` unconditionally would be worse than either. On a
  conventional subnet a mistyped gateway currently fails at route-add time,
  loudly, which is principle 5. With the flag always on it would install a
  route that silently blackholes instead.

Alternatives that also work but were not taken: an explicit link-scope `/32`
route to the gateway before the default route (one extra route, same effect),
and a gateway-less default route on the device, which additionally requires
`proxy_arp` on the host tap and makes the guest ARP for every off-host
destination. Both are measured in fctl RESEARCH.md §7.

Always bring `lo` up. Failing to do so breaks a surprising amount of software.

**One NIC is sufficient for MMDS.** Firecracker's own documentation attaches
MMDS to `eth0`; a dedicated MMDS interface is unnecessary. fcm currently
creates a second TAP (`mmds0`) — see
[fctl/docs/fcm-gap.md](../fctl/docs/fcm-gap.md).

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

Per §4.3. Note that §5.1–5.5 run only for the **apply** profiles (`zpinit`,
`command`). For the generate profiles, vminit skips straight from §5.1 to
rendering config per §4.4 and then execs — it must not mount anything.

For **`zpinit`**, write the composed env to `/run/zpinit/env` in zpinit's
`key=value` format first — `/run` is a tmpfs mounted in §5.2, so this is cheap
and disappears on shutdown. Then `execve`. Never `fork`; vminit must not
linger, and zpinit's reaper is written on the assumption that it is PID 1.

The composed env additionally travels in **`envp`**, and that is the primary
channel of the two. Three reasons, all found while implementing:

- `envp` *is* zpinit's "container env" tier — the same tier `docker run -e`
  and Dockerfile `ENV` land in — which is precisely what image env is. Routing
  it only through the env file would place it in the `entrypoint.d` tier
  instead, above the operator's `[env]` globals in `zpinit.toml`.
- the env file is line-based (`internal/entrypoint/entrypoint.go`), so it
  cannot represent a value containing a newline. Such a variable is skipped
  from the file with a warning and reaches zpinit through `envp` regardless,
  rather than silently corrupting every line after it.
- the file still earns its place: it is what makes the composed env visible to
  `entrypoint.d` scripts, and what they append to in order to hand variables
  forward. It is written mode `0600`, because image env routinely carries
  credentials and everything that legitimately reads it runs as root.

**`boot.args` is not defaulted**, despite `["--vm"]` being the right production
value. zpinit releases before VM mode exists reject unknown flags, so injecting
the flag would turn a working boot into a failing one. `vmpack` writes it;
vminit warns at boot when it is absent, because the symptom otherwise appears
much later as a kernel panic at shutdown with nothing pointing at the cause.

**PID 1's stdio has to be set up before the handoff.** Production boots without
`console=` (§7.2), so the kernel may hand init closed descriptors — and a child
inheriting a closed fd 1 gets it assigned to the first file it opens. zpinit
logs to `os.Stderr`, so without this its output is simply lost. vminit points
fd 0 at `/dev/null` and fds 1 and 2 at the log target before exec'ing.

For the **generate profiles**, `execve` `boot.init` directly.

For **`command`**, vminit stays alive as PID 1 and additionally needs:

- `reboot(LINUX_REBOOT_CMD_CAD_OFF)` at startup, so Ctrl+Alt+Del arrives as
  SIGINT to PID 1 instead of triggering an immediate kernel reset
- a `wait4(-1, WNOHANG)` reaper loop for orphans reparented after a double-fork
- signal forwarding to the child's process group, honouring
  `image.stop_signal`
- on child exit: publish the status (§7 logging), `sync()`, unmount
  `config.mounts`, then `reboot(RB_AUTOBOOT)`

This is the same lifecycle zpinit implements for `profile = "zpinit"`
(see `ZPINIT_CHANGES.md` §2, §5). Keep the two implementations behaviourally
identical so a VM behaves the same regardless of profile.

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

## 7. What vminit needs from the kernel and cmdline

**The guest kernel and the kernel command line belong to [fctl](../fctl).**
Console handling in particular is a host-side runtime flag, never a property of
the image. The authoritative documents are
[fctl/docs/guest-kernel.md](../fctl/docs/guest-kernel.md) and
[fctl/docs/booting-vms.md](../fctl/docs/booting-vms.md); this section states
only what vminit *depends* on, so that a change over there which breaks vminit
is recognisable as such.

### 7.1 Kernel options vminit relies on

```
CONFIG_VIRTIO_BLK=y  CONFIG_VIRTIO_NET=y  CONFIG_VIRTIO_MMIO=y
CONFIG_EXT4_FS=y  CONFIG_DEVTMPFS=y     # §5.2 mounts assume devtmpfs
CONFIG_IP_PNP=y                         # only for the ip= fallback below
CONFIG_SERIAL_8250=y  CONFIG_SERIAL_8250_RUNTIME_UARTS=1
                                        # logging target = serial, §4.2
CONFIG_SERIO_I8042=y  CONFIG_KEYBOARD_ATKBD=y
                                        # §5.6 SIGINT handoff needs CAD to arrive
CONFIG_VSOCKETS=y  CONFIG_VIRTIO_VSOCKETS=y   # only if host-side exec lands
```

> The i8042 pair is **not** in the checked-in `guest_configs/*.config` files —
> it comes from `ci.config`, which the build script concatenates on top. Build
> from the base config alone and `SendCtrlAltDel` silently stops working, which
> takes the §5.6 SIGINT path with it. See
> [fctl/docs/guest-kernel.md §1](../fctl/docs/guest-kernel.md).

### 7.2 Cmdline vminit assumes

- **`init=/sbin/vminit`** — obviously.
- **No `console=` in production.** vminit logs to `/dev/ttyS0` directly, and
  the port is enumerated with or without it on x86_64 (verified). Without
  `console=` the port is also uncontended by a getty, which is what the logging
  design wants.
- **`reboot=k`** — vminit's reboot path (§5.6) relies on a guest reset exiting
  the VMM.
- **Never `i8042.nopnp`** — it breaks `SendCtrlAltDel`, and with it vminit's
  only graceful-shutdown signal.
- **`ip=…` is optional**, and under the routed `/32` model it is not merely
  optional but impossible. The kernel's `ipconfig` initialises its netmask to
  `INADDR_NONE`, which is bit-for-bit `255.255.255.255`, so **a `/32` netmask
  is indistinguishable from "unset"**: it silently guesses a classful mask and
  then rejects the gateway as off-link. Verified —
  [fctl RESEARCH.md §7](../fctl/docs/RESEARCH.md). fctl therefore omits it from
  production boot args and restores the belt-and-braces property with a
  debug-only second NIC on a private `/30`. Where `ip=` does still apply
  (that debug NIC, or a conventional subnet) it is IPv4 only, one interface,
  and it does not write `/etc/resolv.conf` — DNS lands in `/proc/net/pnp`.
  Leaving it off the real interface is also what makes the boot test's
  connectivity check meaningful: with `ip=` the guest would reach the host
  whether or not §5.3 works at all.
- **`root=` and `rw` do not need to be passed.** Firecracker appends
  `root=/dev/vda rw` itself, after whatever `boot_args` the caller supplied
  [VERIFIED] — a guest booted with `boot_args` of only
  `reboot=k panic=1 pci=off init=/sbin/vminit` sees both in `/proc/cmdline`.
  vminit still checks and remounts (§5.2), because this is fctl's to change.

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
   - `/usr/local/bin/zpinit` + `zpctl`, if `profile = "zpinit"`
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
- **All limits are host-enforced** — vCPU, memory, disk and network
  throughput, traffic policy, jailer, seccomp. **vminit enforces nothing**, and
  none of it is vminit's to configure. See
  [fctl/docs/](../fctl/docs/) for how the host side does it.
- **MMDS V2 only.** V1 is unauthenticated; any process in the guest, including
  one reached via SSRF in the tenant's own app, can read it. vminit's agent
  must therefore do the token dance (§6) and should refuse to fall back to V1
  even if the host misconfigures it.

---

## 10. What vminit needs from the host

vminit's host-side requirements — boot args, `/etc/vminit/override.json`
placement, MMDS shape and version, graceful stop, serial log destination — are
owned and tracked by [fctl](../fctl). The gap list against the existing
`firecracker-manager` lives in
[fctl/docs/fcm-gap.md](../fctl/docs/fcm-gap.md).

The contract is three items wide, and vminit owns the schema of two of them:

| Interface | Owner of the mechanism | Owner of the schema |
| --- | --- | --- |
| Kernel cmdline | fctl | fctl (§7 states what vminit assumes) |
| `/etc/vminit/override.json` | fctl writes it pre-boot | vminit (§4.2) |
| MMDS contents | fctl populates it | vminit (§4.2, §6) |

---

## 11. Testing

Most of this cannot be tested on macOS. Plan for a Linux host with KVM.

`test/run.sh` is the harness: it builds a fixture rootfs per scenario, boots it
under Firecracker, and has an in-guest probe check the resulting machine
against the expectations. The fixture generates the vminit config *and* the
probe's expectations from one set of shell variables, so the two cannot drift.
It stands in for `vmpack` until `vmpack` exists, and the rootfs it produces is
deliberately `scratch`-like: three static Go binaries, no shell, no libc.

Three scenarios pass on real hardware [VERIFIED]:

| scenario | proves |
| --- | --- |
| `base` | the full §5.2–§5.6 path and the zpinit handoff, from `config.json` alone |
| `override` | the §4.1 merge on a running machine: `hostname` and `user_override` replaced, `extra_env` merged per variable, the network and image untouched by an override that mentions neither |
| `routed32` | fctl's routed-address model end to end: a bare `/32` with no on-link subnet, a link-local gateway shared by every tap, and the `RTNH_F_ONLINK` default route that makes it work (§5.3) |

`routed32` earns its place by failing without the flag rather than by passing
with it; the negative control is quoted in §5.3. The end-to-end HTTP fetch is
what detects the difference, because a guest with an address but no default
route still passes every check that only reads local state.

`build-rootfs.sh` writes `test/net.env` and `boot.sh` sources it, so the TAP
the host configures comes from the same variables as the guest's expectations.
That extends the fixture's existing anti-drift property to the host side,
which the `/32` scenario needs because it differs in the *shape* of the host
setup and not merely in the numbers.

The probe checks the mount table *and its flags*, the `/dev` symlinks, the
identity files, the address and default route, env precedence, `HOME`/`TERM`
defaulting, `/run/zpinit/env` and its mode, the resolved uid/gid, that PID 1 is
zpinit and the workload is not, and an HTTP fetch of a host listener bound to
the TAP address — which is the end-to-end proof that netlink configuration
worked, since the boot args deliberately carry no `ip=`.

Guest-visible boot arrives at the probe's report ~0.74s after the Firecracker
process starts, of which vminit's own bringup is 14-28ms.

- **Unit** — config merge precedence, OCI→zpinit TOML translation, env
  composition order, user resolution incl. numeric ids and missing `/etc/passwd`.
- **`vmpack` golden tests** — pack `alpine`, `debian`, `nginx:alpine` and a
  `scratch` image; assert tree contents, injected files, and determinism
  (pack twice, compare hashes).
- **Boot matrix** — the same four images must boot, get an IP, resolve DNS,
  and run their entrypoint. `scratch` is the important one: it proves zero
  guest-userspace dependency.
- **Shutdown** — `SendCtrlAltDel` reaches vminit as SIGINT (§5.6), which stops
  the workload and reboots; assert no `Attempted to kill init` panic. The
  host/kernel half of this path is already confirmed against stock systemd
  ([fctl RESEARCH §3](../fctl/docs/RESEARCH.md)); what remains untested is
  vminit as PID 1, which is the half this repo owns.
- **Boot time** — measure **from the host**, never with in-guest tooling; see
  [fctl/docs/performance.md](../fctl/docs/performance.md) for the baseline and
  why the guest's own clock lies during early boot. vminit's contribution to
  cold start is the interval from `init=` exec to handoff, which it should
  report itself by signalling Firecracker's boot-timer device.
- **Agent** — `PATCH /mmds` while running, assert the change is applied and
  survives a reboot.

---

## 12. Deferred

Not now, but nothing here should be designed against:

- **aarch64** — no i8042, so shutdown needs a different trigger; the serial
  device is MMIO and *is* conditional on `console=`
- **The `systemd` and `openrc` generate profiles.** The `cloud-init` profile
  is in scope and nearly free, and §4.5 argues it should cover every
  self-built distro image. Build these two — and the three `network` variants
  under `systemd` — only when a specific image cannot take cloud-init.
- **Host-side `exec`** — needs `zpctl exec` and a vsock listener; see
  `ZPINIT_CHANGES.md` §9 and §6. Note vsock is the one device that crosses the
  VM boundary, so it is a security decision as much as a feature.

### Deferred to fctl, but with consequences here

These are host-side capabilities tracked in
[fctl/docs/performance.md](../fctl/docs/performance.md). They are listed here
only because of what they would do to vminit's design:

- **Snapshots, and VMGenID.** Restoring N VMs from one snapshot means duplicate
  machine ids, entropy pools, RNG seeds, ASLR layouts and pre-snapshot key
  material, plus vsock UDS collisions. Upstream's position is that resuming the
  same state more than once is **insecure** without a uniqueness mechanism, and
  that mechanism is the **VMGenID** device: Firecracker changes a 16-byte
  identifier and injects an interrupt on every resume (Linux 5.18+, ACPI).

  Detection without reaction buys nothing, and the reaction is **vminit's**:
  on a VMGenID change, re-seed the RNG and regenerate everything assumed
  unique — machine-id, SSH host keys, any cached credentials. That is a
  standing daemon responsibility, not a boot-time one, so it likely belongs in
  `vminit agent` rather than `vminit boot`.

  This is the case where §5's premise breaks: vminit's job changes from
  "configure a fresh machine" to "repair a cloned one". Decide before §5 is
  built, not after. Under principle 6, snapshots stay off until the VMGenID
  path exists end to end.
- **virtio-pmem + DAX as the shared lower layer.** Denser than the overlay
  design now specified in §5.2, because the guest maps host page cache directly
  instead of copying through the block device — but for the same reason it is
  weaker, and fctl has it as conditional-only. If adopted it changes the root
  device and mount handling in §5.2 (`/dev/pmem0`, `rootflags=dax`); the CI
  kernel already has `DAX` and `FS_DAX`, so nothing blocks it technically.
  Under principle 6 it needs the density measurement to justify the weaker
  isolation first. The plain squashfs+overlay form is **no longer deferred** —
  it is specified in §5.2.

---

## 13. Milestones

**M1 — `vmpack` produces a bootable rootfs.**
Pull, extract, clean, size, format. Inject a hand-written config. Boot it with
a stub vminit that only mounts and execs `/bin/sh`. Proves the pipeline.

**M2 — `vminit boot` full bringup.**
Mounts, netlink, identity files, user resolution, env composition, handoff in
`command` profile (which also exercises the reaper and reboot path). Success =
`nginx:alpine` boots, serves traffic, resolves DNS, and stops cleanly on
`SendCtrlAltDel`.

**M3 — zpinit handoff.** *(bringup and handoff done; shutdown blocked on
zpinit)*
`profile = "zpinit"`, generated service TOML, `/run/zpinit/env`. Requires
zpinit sections 1, 2, 4 and 5 from `ZPINIT_CHANGES.md`. Success = a
multi-process image with restarts and readiness probes.

The vminit half is built and verified against zpinit v0.5.5: full bringup,
`/run/zpinit/env`, `execve`, and zpinit supervising the workload as PID 1 with
the composed environment. What is left needs the zpinit side. Until
`ZPINIT_CHANGES.md` §1 and §2 land, zpinit runs in container mode, so a service
that exits takes PID 1 with it and panics the kernel — the boot test works
around it by holding the workload open, and `SendCtrlAltDel` still hard-resets
the VM because nobody has called `CAD_OFF`. vminit deliberately does not call
it on this path: that would deliver SIGINT to a zpinit that treats it as
"exit", which is worse than the hard reset. It stays zpinit's, per
`ZPINIT_CHANGES.md` §5a.

**M4 — `scratch` and the boot matrix.**
All four test images. This is the real proof of distro independence.

**M5 — `vminit agent` and MMDS.**
Live reconfiguration, V2 tokens, persistence across reboot.

**M5b — the `cloud-init` profile, cheap.**
Boot a self-built Ubuntu image with `profile = "cloud-init"` and MMDS populated
in EC2 format. Almost no vminit code beyond writing the datasource pin and
staying out of the way — mostly an fctl and MMDS-shaping task, which is why it
is worth doing early as validation of §4.5.

**M6 — production hardening.**
Jailer, rate limiters, console-off boot args, `serial_out_path` — all of it
now tracked in [fctl](../fctl), see §10.

M1 and M2 are the ones that retire the most risk. Everything after is additive.

---

## 14. Open questions

1. ~~**Does `/dev/ttyS0` work with `console=` omitted on x86_64?**~~
   **Settled 2026-08-06: yes.** The code was right, the swagger doc is wrong
   for x86_64. `PUT /serial` with `serial_out_path` also works with `console=`
   absent, so `logging.target = "serial"` stands and the log path stays on the
   UART — `ZPINIT_CHANGES.md` §7 needs no revisiting. Evidence, now host-side,
   in [fctl/docs/RESEARCH.md §2](../fctl/docs/RESEARCH.md). Fallout:
   `i8042.nopnp` must never appear in the boot args, because it breaks
   `SendCtrlAltDel` and with it vminit's SIGINT shutdown path
   ([fctl RESEARCH §3.1](../fctl/docs/RESEARCH.md)).
2. **Rootfs sizing policy** — fixed slack, percentage, or caller-specified?
   Interacts with whether `/data` is a separate volume.
3. **Does `vmpack` belong in this repo long-term?** Sharpened now that
   [fctl](../fctl) exists: `vmpack` is host-side tooling, which is fctl's
   half of the world, and fctl is what will invoke it. Argument for keeping it
   here anyway: it writes the config schema `vminit` reads, and drift between
   the writer and the reader is the likeliest bug in the whole design. Left
   here for now on that basis — the co-location *is* the mitigation. Revisit
   when fctl has a shape.
4. **How does `user_override` reach the workload under `profile = "zpinit"`?**
   Found while implementing §5.5. Under this profile vminit never `setuid`s —
   it execs zpinit as root, and the uid the workload runs as comes from `user`
   in the service TOML, which `vmpack` baked in at image build time. So a
   per-launch `user_override` currently changes only the composed environment
   (`HOME`), not the uid: the `docker run --user` equivalent is silently
   half-applied.

   Three candidate answers, none obviously right yet:

   - vminit writes a zpinit drop-in under `/etc/zpinit/services/` overriding
     `user`. Cheap, but puts knowledge of zpinit's config schema in vminit,
     which so far only `vmpack` has.
   - `vmpack` emits `user` as something zpinit resolves at run time from the
     env vminit composes. Needs a zpinit feature and interpolation in configs,
     which zpinit's `CLAUDE.md` lists as a non-goal.
   - declare `user_override` unsupported for this profile and reject it at
     merge time, which at least fails loudly instead of half-working.

   The same question applies to `exec_override` and `working_dir`, which live
   in the baked TOML for exactly the same reason. Note it does **not** arise
   for `profile = "command"`, where vminit forks the workload itself and can
   apply all three directly. Decide before `vmpack` generates service files.

5. **Agent poll interval, or push?** Polling MMDS every 30s is simple but
   laggy. A vsock nudge from the host would be immediate — but that is the
   same vsock work as host-side exec, so it may come for free later.
