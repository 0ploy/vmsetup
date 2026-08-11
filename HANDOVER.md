# Handover from the fctl session

Written 2026-08-11 from `fctl/`, against `fctl/PLAN.md` as it stands after the
pre-implementation review. This is the fctl side asking the vminit side for
decisions on the **interfaces the two projects meet at** — which have moved
since vminit's plan was written, mostly because the shared squashfs + per-VM
overlay design changed what is possible.

It is meant to be implementable directly, in a session working from `vminit/`.
Where it proposes a contract, it gives the exact format and the exact fields.

Nothing here blocks vminit from continuing. But §1, §2, §3 and §5 all follow
from the shared-lower design, and fctl's M2 hardens the rootfs creation path —
so answers before then avoid a retrofit.

Each section states what fctl needs, why, what fctl does in the meantime, and
what changes if you decline. Declining is a real option in every case; the
purpose of the document is to make the choice explicit rather than to have one
side quietly assume. Two of these — §1 and §6 — make vminit *smaller*.

| § | Ask | Owner of the answer | Blocks |
| --- | --- | --- | --- |
| 1 | **The config path: one document, raw bytes on a block device, no filesystem.** `/etc/vminit/{config,override}.json` both deleted | vminit (schema + boot order) | fctl M2 creation path |
| 2 | Overlay `workdir` must live inside the upper filesystem | vminit (§4.2 example is wrong) | correctness, guest-side |
| 3 | The overlay root arrives mounted `ro`, not `rw` | vminit (§5.2 doc claim) | nothing, but the doc misleads |
| 4 | A shutdown-intent sentinel on `/dev/ttyS0` | vminit (§5.6 shutdown path) | fctl restart classification |
| 5 | `vmpack` emits squashfs + a manifest, and the manifest becomes the **authoritative** image config | vminit (`vmpack` is yours) | fctl M4; §5.1 is coupled to §1 |
| **6** | **vminit stays PID 1 — never `execve`s. It owns `exec` over vsock, and rescue mode** | **vminit** | **`fctl exec`, M5** |
| 7 | `ip=` is gone entirely; every VM has one interface | vminit (§7.2 wording) | nothing, but the doc is now wrong |
| **8** | **The profile axis disappears.** Distro images are no longer vminit's concern | **vminit (schema)** | nothing — pure deletion |

**§6 and §8 are the big ones.** Together they remove five of the six changes
zpinit was going to need
and most of vminit PLAN §4.

---

## 1. The config path: one document, raw bytes, no filesystem

A net reduction in vminit: a merge disappears, a mount disappears, and a file
disappears. Everything here is fctl's proposal for a contract vminit owns —
push back on any of it.

### 1.1 What changes, in one table

| | Today (vminit PLAN §4) | Proposed |
| --- | --- | --- |
| Documents vminit reads | 2: `/etc/vminit/config.json` + `/etc/vminit/override.json` | **1** |
| Merge in the guest | 3 rules (shallow per key; `env` per variable; `exec_override` replaces two other fields) | **none** |
| Where it comes from | files in the rootfs | **a block device, raw bytes** |
| Filesystem on that device | — | **none.** No mount, no mkfs, no fs driver |
| When it can be read | after the rootfs is available | **before anything is mounted** |
| `/etc/vminit/config.json` | baked by `vmpack` | **deleted. Not in the image at all** |
| `/etc/vminit/override.json` | written by fctl per boot | **deleted** |

### 1.2 Why one document

fctl composes the whole thing per launch — image-derived fields from `vmpack`'s
manifest, per-launch fields from the VM spec — and vminit parses one document
and acts on it.

The point is to delete the **merge**. It is the one place where the config's
writer (fctl) and its reader (vminit) must agree on semantics, and it currently
has three different rules in one document. The workspace `CLAUDE.md` names drift
between writer and reader as *"the likeliest bug in the design"*, and this is
where that bug would live. Deleting the merge is worth more than specifying it
more carefully.

fctl is not inventing image semantics to do this. `vmpack`'s manifest is already
a hard requirement for artifact import and already carries the OCI-derived
config; fctl forwards it and applies the per-launch deltas it already owns.

**`/etc/vminit/config.json` goes away entirely** — not "kept as a fallback".
A file that is read *sometimes* is a second code path and a second thing that
can be stale, which is the drift we are removing. `vmpack` stops writing it.
The image becomes a filesystem and nothing else; all config is runtime.

The honest cost: **a rootfs alone can no longer boot.** You need a config blob
too. For a system where nothing boots without fctl that is cheap, but it does
remove the "mount the image and boot it by hand" affordance, and it is the main
reason to say no.

### 1.3 The blob format

The device carries **raw bytes**. Not ext4, not any filesystem.

```
offset  0   "FCTLCFG\0"        8 bytes, magic
offset  8   version            u32 little-endian, starts at 1
offset 12   length             u32 little-endian, bytes of JSON that follow
offset 16   JSON document      NUL-padded to the end of the device
```

- Device size 64 KiB, read-only. fctl validates the composed document against
  that ceiling host-side, so the guest never meets a truncated document.
- vminit: open the device, read 16 bytes, check the magic, check the version,
  read `length` bytes, `json.Unmarshal`. That is the entire config path.
- `head -c 4096 /dev/vdb` shows you the config after the header. Arguably more
  inspectable than a filesystem, and it satisfies the "a human with `cat` can
  answer what this is" principle both projects hold.

No `mkfs`, no staging directory, no loop mount on update, no mount ordering, no
filesystem-driver dependency, no fsck or corruption semantics.

**The ordering property is the one to notice.** `/dev` is already populated
before `init=` runs — the kernel mounts devtmpfs itself
(`CONFIG_DEVTMPFS_MOUNT=y`, your §5.2) — so reading the config has **no
prerequisites at all**. vminit can read its config as its first action, before
mounting `/proc`, `/sys` or anything else. That is exactly the ordering the
rescue-mode design in §6 needs: read config → start the vsock agent → *then*
begin bringup, so a bringup failure still leaves an inspectable machine.

### 1.4 Finding the device — no scanning

Every *other* device is simply named in the document: it says the lower is
`/dev/vda`, the upper `/dev/vdc`, volume `data` is `/dev/vdd`. fctl composes it
knowing exactly which `PUT` order it used, so it is self-consistent by
construction. **The config device is the only one that has to be found**, since
it is the one thing that cannot be named in the file it contains.

Three independent things fix it, and vminit should require all three to agree:

1. **Position — always the first non-root drive.** Firecracker's
   `BlockBuilder::insert` `push_front`s the root device and `push_back`s the
   rest in `PUT` order. fctl issues the config blob first among the non-root
   drives, so it is `/dev/vdb` in both storage modes:

   `vda` is the squashfs (the root device, `push_front`ed), `vdb` the config,
   `vdc` the overlay, `vdd`+ named volumes. Volumes append after and cannot
   disturb it.

2. **Pointer — `vminit_config=/dev/vdb` on the kernel command line.** fctl knows
   what it attached, so vminit derives nothing. This is the authoritative one:
   it survives a future virtio-pmem root, where there is no root *block* device
   and the letters shift.

   Two notes. Reading it means mounting `/proc` before the config read — a
   trivial reorder, `/proc` depends on nothing. And **there is deliberately no
   dot in the parameter name**: dotted parameters are parsed as module
   parameters and may be dropped rather than reaching init, so read
   `/proc/cmdline` rather than looking in your environment. fctl will confirm
   the exact kernel behaviour on its first spike; if `/proc/cmdline` turns out
   to be awkward for you, say so and we will find another spelling.

3. **The magic is a check, not a search.** Open what the cmdline says, validate
   the header, and **fail loudly** if it does not match — do not fall back to
   looking elsewhere. A refactor that reorders drives should produce "this is
   not the config device", not a parse of somebody's data volume.

### 1.5 The document itself

Unchanged in content from your PLAN §4.2 as far as fctl is concerned — same
fields, same meanings. What changes is that every field arrives populated, with
no layering: no `exec_override` versus `image.entrypoint` precedence to resolve
in the guest, because fctl has already resolved it.

That does mean the schema can lose the fields that only existed to express
"override the layer below" — `exec_override`, `user_override`, `extra_env` —
collapsing into the plain fields they were overriding. Whether to do that
collapse is yours; fctl can emit either shape. The collapse is cleaner, but it
is a bigger change to your config package, and keeping the names costs nothing
if you would rather not touch them now.

### 1.6 What fctl needs back

1. **Yes or no to the whole shape** (one document, raw blob, no filesystem).
2. **The device path parameter spelling**, if `vminit_config=` and
   `/proc/cmdline` do not suit.
3. **Whether the schema collapses** the `*_override` fields (§1.5).
4. Confirmation that **`vmpack` stops writing `/etc/vminit/config.json`** and
   puts the OCI-derived config in the manifest instead (see §5).

---

## 2. The overlay `workdir` must be on the same filesystem as `upperdir`

**This one is a bug in the plan, not a design choice.** Your PLAN §4.2 example
has:

```jsonc
"overlay_root": {
  "lower":  { "device": "/dev/vda", "fstype": "squashfs" },
  "upper":  { "device": "/dev/vdc", "fstype": "ext4" },   // vdb is the config blob, §1.4
  "workdir": "/overlay/work"
}
```

and §5.2 mounts `upperdir=/overlay/upper/root` alongside it. overlayfs requires
`upperdir` and `workdir` to be **on the same filesystem** — it needs to move
files between them by rename, which cannot cross a mount point. A `workdir` on
the initial root while `upperdir` is on the mounted upper fails the mount with
`EINVAL`, at boot, inside the guest, where it is expensive to diagnose.

**What fctl now specifies** (fctl PLAN §9.3), so that the writer and the reader
agree by construction: the per-VM upper image is formatted with two directories
at its root, and fctl fills both paths into the config explicitly rather than
letting either default.

```
<upper ext4>/
  root/     ← overlayfs upperdir
  work/     ← overlayfs workdir. same filesystem, by construction
```

**The ask:** confirm this layout, and change the §4.2 example so `workdir` is a
path *inside the upper mount* (or make it relative to the upper mount, which is
arguably the better schema — a `workdir` that can name a location outside the
upper is a footgun the schema does not need to offer).

Whichever you pick, fctl creates both directories at `mkfs` time and names both
in the config. There is no version of this where vminit has to guess.

---

## 3. In overlay mode the kernel's root arrives `ro`, and the squashfs is `/dev/vda` by construction

Two corrections to statements in your plan, both consequences of a bug fctl
found in its own §10.2 during review. No action needed beyond the doc fix, but
you should know the mechanism, because your §5.2 currently reasons from the
opposite premise.

**fctl's error was to specify `is_root_device: false` on both drives**, on the
theory that the real root is the overlay vminit assembles. That does not boot.
Firecracker emits `root=` *only* for a drive flagged as root —
`attach_block_devices` calls `append_root_device_cmdline` under
`if locked.root_device()` (`src/vmm/src/builder.rs`,
`src/vmm/src/vmm_config/boot_source.rs`). With neither drive flagged and no
`initrd_path`, the kernel has no root filesystem, panics at
`VFS: Unable to mount root fs`, and never reaches `init=`. Now corrected: the
**squashfs lower is the kernel's root device**, the kernel execs `/sbin/vminit`
out of it, and vminit assembles the overlay from there — the same shape as
upstream's `overlay-init`, minus the shell.

Two things follow for vminit:

- **`root=/dev/vda ro`, not `rw`.** The same function appends `ro` when the root
  drive is `is_read_only: true`, which a shared squashfs always is. Your §5.2
  says *"it appends `root=/dev/vda rw` by itself [VERIFIED]"* — that
  verification was on the writable-ext4 path and holds only for
  `mode: "writable"`. In overlay mode the initial root is read-only and cannot
  be remounted `rw` (squashfs), so the "check and remount `/`" step will fail
  there. Your existing behaviour is already correct — failure to remount is a
  warning, not an abort, and everything in §5.4 writes after the pivot into the
  writable overlay — but the doc's stated reason is wrong and worth fixing
  before someone relies on it.
- **Device naming is now guaranteed, not conventional.**
  `BlockBuilder::insert` (`src/vmm/src/vmm_config/drive.rs`) `push_front`s the
  root device and appends the rest in `PUT` order. Flagging the squashfs is what
  makes it `vda`; fctl issuing the config blob first among the non-root drives
  is what makes *that* `vdb` (§1.4). The overlay lands at **`vdc`**, and named
  volumes at `vdd`+.

  Previously your `overlay_root` naming `/dev/vda` and fctl's `PUT` order agreed
  by luck. They now agree by construction — and note that with §1 the whole
  question mostly dissolves, because fctl composes the document and names in it
  whatever devices it actually attached. Only the config device itself needs
  finding.

---

## 4. A shutdown-intent sentinel on `/dev/ttyS0`

**The ask:** immediately before vminit resets the VM, print one fixed line to
the serial log stating *why* — reboot or shutdown. Something like:

```
vminit: reset intent=shutdown
vminit: reset intent=reboot
```

Exact spelling is yours; fctl will match whatever you pick, and treats an
absent line as "unknown", which is today's behaviour.

**Why.** Firecracker exits the VMM process on CPU reset, so a guest ends its own
VM by calling `reboot()` — and that is the *same event* whether the guest meant
"restart me" or "I am done". There is no in-band way to tell them apart, and
your own shutdown path uses `reboot()` to end the VM (§5.6). fctl has to guess,
and it guesses "reboot", which means a VM that meant to shut down comes back.

It got sharper during review. Production boot args carry `panic=1`, so a
**panicking** guest also reboots — CPU reset, VMM exits status 0, unasked.
Identical to a healthy restart. A guest that panics during boot therefore
restarts forever without ever being counted as a crash. fctl now rate-limits the
in-place restart path to contain that, but the limiter is a backstop; the
sentinel removes the ambiguity for every well-behaved guest, which is nearly all
of them.

**Cost:** one write on a path that already exists, in each project. The shim
already reads the serial log.

**One property worth stating so nobody designs against it later:** the guest is
untrusted and owns its rootfs, so it can forge either sentinel. That is fine —
forging `reboot` buys a restart it could get by rebooting, forging `shutdown`
buys a stop it could get by not restarting. Neither crosses a boundary. The
corollary is that this channel must never be extended to carry anything fctl
would act on more strongly than this.

**Timing:** worth deciding before your §5.6 shutdown path is final, since that
is where the line goes.

---

## 5. `vmpack` must emit squashfs, plus a manifest

Your PLAN §8 step 6 specifies `truncate` + `mkfs.ext4 -d <tree>` — a single
writable ext4 rootfs. In fctl's model that is `mode: "writable"`: supported,
needed for full-distro images, invaluable for debugging, and **not** the density
path. The shared-lower design needs a **squashfs** artifact, which is the thing
that makes one image cost one page-cache copy across every VM booted from it.

**The ask, in fctl's order of need:**

1. `vmpack` produces `rootfs.squashfs` as the default output for registry
   images, with `rootfs.ext4` retained as an explicit `--writable` mode.
2. It emits a **manifest** alongside — fctl stores it at
   `images/<digest>/manifest.json` and will not import an artifact without one.
3. Determinism as already stated in your §8 (same image digest + same vminit
   version ⇒ byte-identical output) matters more for squashfs than for ext4,
   because it is what lets fctl content-address the shared artifact.

### 5.1 §1 promotes the manifest, and that changes step 5 of your pipeline

With `/etc/vminit/config.json` gone from the image (§1.2), **the manifest becomes
the authoritative source of the image's OCI-derived config.** It is no longer
metadata *about* an artifact that also happens to describe it — it is the only
copy, and fctl composes the guest's config from it.

Concretely, your PLAN §8 step 5 ("Inject") loses one bullet:

- ~~`/etc/vminit/config.json` generated from the image config~~ → goes in the
  manifest instead. Everything else injected stays: `/sbin/vminit`, zpinit and
  `zpctl` under the `zpinit` profile, and the generated zpinit service TOMLs.

fctl deliberately does **not** read inside the rootfs to get this. Mounting a
squashfs as root, or shelling to `unsquashfs`, on every `fctl create` is worse
than reading a JSON file sitting next to it.

**One rule on the manifest's contents, and it matters.** Your §8 step 7 has the
manifest carrying *"the recommended boot args"*. **fctl will never read those.**
Boot args are host-side and security-relevant — `i8042.nopnp` silently destroys
graceful stop, `mitigations=off` is a real decision — and fctl's plan already
says they are assembled by fctl and never taken from an image. So:

> fctl takes only **guest-destined** fields from a manifest. Boot args in a
> manifest are advisory documentation for humans; fctl ignores them.

Keep emitting them if they are useful to a reader. Just know nothing consumes
them, so they cannot become a channel by which an image influences the host.

That rule is also what keeps manifest tampering a *guest-local* concern: it can
change what runs inside one VM, which the VM boundary contains and which a
malicious image achieves anyway. fctl records the manifest's digest in the VM
spec for reproducibility — "this VM was created from exactly this manifest" —
not as a security control. Real artifact integrity belongs to the image registry
that will eventually sit in front of all this.

**Timing:** fctl needs squashfs output for M4 (artifact stores and tenancy), not
M2 — M2 can boot a hand-built squashfs. But **§5.1 is coupled to §1**: the
moment `vmpack` stops writing `/etc/vminit/config.json`, the manifest has to
carry the config. Land them together.

---

## 6. vminit is PID 1 for the life of the VM, and owns `exec`

Two changes that turn out to be one: vminit never `execve`s away, and `exec`
moves here from zpinit. The first makes the second nearly free.

### 6.1 Never exec — always fork

vminit PLAN §5.6 currently says: *"Never `fork`; vminit must not linger, and
zpinit's reaper is written on the assumption that it is PID 1."* **The second
half is not true**, and it was checked against the code rather than assumed.

`cmd/zpinit/main.go:79` is the only `getpid()` check in zpinit:

```go
if pid := os.Getpid(); pid != 1 {
    log.Warn("zpinit is not running as PID 1; orphan reaping is unreliable outside containers", "pid", pid)
}
```

A warning, not a refusal — and it names the one and only consequence. The reaper
(`internal/reaper/reaper.go`) uses `syscall.Wait4(-1, …)`, which reaps *your own
children* whether or not you are PID 1; its package doc says PID 1 only **adds**
orphans reparented from elsewhere. Services are registered at `cmd.Start` via
`SpawnTracked`, so they are always direct children and always dispatched
correctly. Signals are plain `signal.Notify` with no PID-1 semantics. There is
no reboot logic in zpinit at all.

So: **vminit forks the handoff target and stays PID 1 forever.** It reaps,
forwards signals honouring `stop_signal`, and on the child's exit publishes the
status, `sync()`s, unmounts and `reboot()`s — exactly the lifecycle §5.6 already
specifies for `profile = "command"`. That path becomes the only path.

Two reapers in one tree do not fight: a process has exactly one parent, so
`wait4(-1)` in vminit and in zpinit operate on disjoint sets by construction.

**This deletes the entire VM-mode feature zpinit was going to need** — see
`zpinit/HANDOVER.md`. zpinit
runs in container mode, unmodified. §4 is the nicest: `execCmd` at
`main.go:371` replaces zpinit's image with the app, fatal when zpinit is PID 1
and harmless when it is a grandchild of vminit.

Two things for the boot matrix rather than reasoning:

- a service that double-forks — the orphan reparents to vminit instead of
  zpinit. Both reap it, no zombie either way, untracked in both cases. Worth one
  test.
- **PID 1 signal semantics cut the other way.** The kernel drops signals to
  PID 1 that have no handler; a non-PID-1 zpinit will instead *die* on an
  unhandled signal. zpinit handles TERM/INT/HUP/QUIT and discards USR1/USR2, so
  the exposure is small, but it is a real behavioural difference.

### 6.2 The vsock listener

Because vminit never goes away, the listener is a goroutine in a process that is
already resident. No forked agent, no surviving an image replacement, no
handover.

fctl's side (PLAN §11.6):

- `PUT /vsock` with `uds_path` inside the jail, gated on `devices.vsock`,
  default on. Pre-boot only — it cannot be attached to a running VM.
- fctl connects as a client, writes `CONNECT <port>\n`, expects `OK <port>\n`,
  then speaks your protocol. **It never listens**, and never creates a
  `<uds_path>_<port>` socket, so guest-initiated connections terminate inside
  the VMM and reach no host process. That invariant is what makes vsock
  acceptable — [fctl/docs/threat-model.md](../fctl/docs/threat-model.md).
- The guest kernel carries `CONFIG_VSOCKETS` + `CONFIG_VIRTIO_VSOCKETS`
  fleet-wide.

**Why exec is vminit's and not zpinit's:** its work is resolving a user to
uid/gid/groups, applying the composed image environment and working directory,
then forking — your §5.5, verbatim. `CLAUDE.md` is explicit that *"zpinit needs
no OCI awareness whatsoever."* It also answers your PLAN §14 Q4, where
`user_override` is silently half-applied because vminit never `setuid`s: owning
exec makes vminit the component that knows how to become a user in this machine.

Optionally the same resident process can poll MMDS for live config updates, when
fctl has enabled the device. Off by default on both sides.

### 6.3 Rescue mode

Start the listener **before** bringup. It needs nothing mounted — `/dev` is
populated by the kernel and the config arrives on a block device (§1.3).

Then a bringup failure need not be fatal: report it and **park**, listener
alive, so the machine can be inspected over `fctl exec` with no address, no
route, no sshd and no shell in the image.

Two things to get right:

1. **Parking must not look like success.** fctl will surface parked distinctly
   from running and exempt it from restart policy, but it needs a signal to do
   that — propose what you want to emit. Pairs naturally with §4's sentinel.
2. **Rescue is not a degraded workload mode.** Park, do not retry, do not
   continue to handoff. The machine is broken; it exists to be looked at and
   then destroyed.

### 6.4 What fctl needs back

1. The **port number**, or the config key carrying it.
2. The **wire protocol**: stdio multiplexing, PTY vs pipes, window resize,
   signal forwarding, exit status, and what happens when the connection dies
   mid-command (kill the child, or orphan it?). Closest prior art is
   `fly-io-init/src/api/exec.rs` and `src/pty/mod.rs`.
3. **Which environment `exec` gets** — the image's, or a specific service's.
   Same question as your PLAN §14 Q4; answer it once for both.
4. The **parked-state signal** (§6.3).

### 6.5 Scope

fctl's device and the connect-only invariant are M2 and depend on nothing from
you. `fctl exec` is M5. Rescue mode is the part with real design content and
should be settled before §5's bringup error handling is final — "park instead of
abort" changes how failure is handled rather than adding to it.

---

## 7. `ip=` is never emitted, and every VM has one interface

Your PLAN §7.2 says `ip=` survives on "that debug NIC, or a conventional
subnet". Drop that clause: fctl never emits `ip=`, and every VM has exactly one
network interface.

Reaching a VM whose bringup failed is `fctl exec` over vsock plus the serial
log (§6.3) — neither needs an address, a route, or a listener in the image.

Your MAC-pinning logic in §5.3 stays correct and worth keeping on its own
merits; it is simply not load-bearing for a second interface any more.

---

## 8. The profile axis disappears

Pure deletion, and the largest single reduction in this document. It follows
from two things already settled: vminit never `execve`s (§6.1), and distro
images are no longer vminit's concern.

### 8.1 Distro images leave

A full-distro VM — systemd as PID 1, its own package set, its own `sshd` —
**needs nothing from vminit**. There is no overlay to assemble (it uses a single
writable ext4 the kernel mounts directly), no mounts to make, no netlink,
no env to compose, no supervisor to hand off to. fctl boots it by setting
`init=/sbin/init` and attaching one drive.

So vminit is not in those images at all. fctl has moved the whole case to a
later phase in [fctl/docs/distro-vms.md](../fctl/docs/distro-vms.md), out of its
v1 plan.

Note this is also the only path verified on real hardware so far — fctl's
RESEARCH test bed is Ubuntu 24.04 + stock systemd, no vminit — so nothing is
being thrown away, only relocated.

### 8.2 What that deletes from vminit PLAN §4

| Delete | Why |
| --- | --- |
| `boot.profile` (`zpinit \| command \| cloud-init \| systemd \| openrc`) | Two of the five were distro-only and deferred; a third (`cloud-init`) is distro-only and leaves; the remaining two differ only in what path to run |
| `boot.network` (`auto \| netplan \| systemd-networkd \| ifupdown`) | Only read under `profile = "systemd"` |
| §4.4 generate profiles, §4.5 "just install cloud-init", §4.6 | All about images vminit is no longer in |
| The `/etc/cloud/cloud.cfg.d` datasource pin | An image property. If an image ships cloud-init, it should carry cloud-init's config |
| All MMDS *requirement* | See §8.4 — it becomes optional, not mandatory |

### 8.3 What replaces it

```jsonc
"handoff": { "path": "/usr/local/bin/zpinit", "args": [] }
```

That is the whole thing. `zpinit` and `command` stop being profiles and become
"what did you put in `path`" — either zpinit or a Docker workload command.
Bringup is always the same and always complete: mounts, config, rootfs, network,
identity, env. There is no matrix.

Note `args: ["--vm"]` from the old examples is now vestigial — there is no VM
mode in zpinit to enable (§6.1). Drop it, or make it `--supervised` if that
route is taken.

### 8.4 MMDS becomes optional, and vminit may ignore it entirely

MMDS had two jobs: feeding cloud-init on distro images, and live updates to the
vminit agent. The first leaves with §8.1. The second is better served by
`fctl exec` — push rather than poll, immediate rather than a poll interval,
bidirectional rather than read-only.

fctl now gates it on `devices.mmds`, **default off**, because enabling it is not
free: `is_mmds_frame` runs on every transmitted frame, and MMDS traffic is
exempt from the network rate limiter.

So: vminit's agent *may* poll MMDS for live config when the device is present,
and that is an optional capability rather than a required one. If it is simpler
not to build it in v1, nothing in fctl depends on it.

### 8.5 The one thing this forecloses

A fleet of identical distro VMs sharing one squashfs would be a real density
win, and it needs something to assemble the overlay before systemd starts — i.e.
vminit with an empty bringup and an `execve` handoff, which is exactly what is
being deleted.

If that requirement ever appears it comes back as a minimal bringup mode, added
for a concrete case with a concrete shape. It is additive to the schema, so
nothing here has to be undone first.

---

## 9. No reply needed — just so you know

These are fctl-side decisions that change something visible inside the guest.
None of them asks anything of vminit; they are here so the next surprise is not
a surprise.

- **fctl's `iface_id` is `net0`, not `eth0`.** It is fctl's host-side label for
  the interface — it appears in `PUT /network-interfaces/<id>` and in the MMDS
  config — and it has never had any relationship to what the guest kernel names
  the device. It was renamed so that nobody reads a host-side log line as a
  statement about guest naming. The guest still sees `eth0`; there is only one
  interface (§7).

- **A virtio-rng device is now attached to every VM.** `PUT /entropy` is
  pre-boot only, so it cannot be added later; fctl attaches it unconditionally
  rather than leaving guests on RDRAND and jitter entropy for early-boot key
  generation. Nothing for vminit to do — the guest kernel wires it up — but if
  you ever gate on entropy availability, it is there.

- **Network and block rate limiters are on by default from fctl M2**, not
  deferred to hardening. Visible in the guest only as bounded throughput.

- **The three `i8042.*` flags are gone from the production boot args.** Measured
  at no benefit end to end, and one typo away from `i8042.nopnp`, which silently
  destroys `SendCtrlAltDel` and with it your SIGINT shutdown path. `i8042` is
  still probed normally, so nothing changes for vminit; your §7.1 dependency on
  `CONFIG_SERIO_I8042` / `CONFIG_KEYBOARD_ATKBD` is unaffected and still
  correct.

- **Serial logs are rotated while the VM runs.** Firecracker opens
  `serial_out_path` once with no `O_APPEND` and never reopens, so fctl rotates
  by copying out and punching a hole rather than renaming. From inside the guest
  this is invisible — `/dev/ttyS0` behaves identically — but the host-side file
  is sparse, so a boot test that reads it should not be surprised by leading
  NULs after a rotation.

- **MMDS keeps exactly one job: the live channel.** With config arriving on a
  block device (§1), MMDS is no longer part of boot configuration at all — it
  carries live updates to a running VM, plus EC2-shaped metadata for the
  `cloud-init` profile, which needs it there by definition. One boot-time
  source, one runtime source, no overlap between them.

---

## What in `vminit/PLAN.md` this touches

Not rewritten here — that is the vminit session's to do, and most of it falls
out of §1, §6 and §8. The list, so nothing is missed:

| Where | What |
| --- | --- |
| §4.1, §4.2 | The three-layer merge and both config files go (§1). `overlay_root.workdir` moves inside the upper mount (§2) |
| §4.3–§4.6 | The profile table, the `network` sub-knob, the generate profiles and the cloud-init argument all go (§8) |
| §5.1 | Config comes from a block device, read before anything is mounted (§1.3) |
| §5.2 | The `[VERIFIED]` claim that Firecracker appends `root=/dev/vda rw` holds only for a writable rootfs; in overlay mode it is `ro` (§3) |
| §5.6 | "Never `fork`" inverts — vminit always forks and stays PID 1 (§6.1). The `command` lifecycle becomes the only lifecycle |
| §7.2 | `ip=` is never emitted; drop the debug-NIC clause (§7) |
| §8 | `vmpack` emits squashfs plus a manifest, stops writing `/etc/vminit/config.json` (§5) |
| §12, §13 | Deferred generate profiles and the M3/M5 milestones that reference zpinit VM-mode work |
| §14 Q4 | Answered by vminit owning `exec` (§6.2) |

Also **`ZPINIT_CHANGES.md` no longer exists** — it moved into the zpinit repo as
`zpinit/HANDOVER.md`, with most of its contents withdrawn. `PLAN.md` references
it at lines 23, 610, 846, 899, 905, 911 and 939, and `docs/RESEARCH.md` at line
262; all are now dangling.

---

## Where the fctl side stands

`fctl/PLAN.md` is the spec and now reflects all of the above; the sections that
reference this document are §6.4, §8.2, §9.3, §9.6, §10.2, §14.3 and §15. fctl
is pre-implementation, starting at M-1 (a throwaway spike that boots one VM
through its own API client) and then M0.

The two documents to read alongside this one are **fctl PLAN §9.6** (the config
path, of which §1 here is the implementable form) and **§14.3** (exec and the
connect-only vsock policy, of which §6 here is the implementable form).

`ZPINIT_CHANGES.md` is gone, replaced by `zpinit/HANDOVER.md` in that repo. The
entire VM-mode feature and the vsock listener are withdrawn; `zpctl exec`
survives only as a container feature. What is left in the guest is one line —
the not-PID-1 warning at `cmd/zpinit/main.go:79`, which now fires on every VM
boot and is misleading.

Reply in whatever form suits — edits to `vminit/PLAN.md` with a note here, or a
section appended to this file. What fctl needs is the decision recorded
somewhere both sessions read, not the form it takes.
