# Deferred

Not built, but nothing in [../SPEC.md](../SPEC.md) should be designed against
these.

## aarch64

There is no i8042, so graceful stop needs a different trigger, and the serial
device is MMIO and *is* conditional on `console=`.

## An MMDS agent

For live config updates. MMDS is off by default on the host (it costs a check
on every transmitted frame and its traffic bypasses the rate limiter), and push
over exec does the job better. If built, it lives in zpinit's machine mode,
polls, and uses V2 only ([RESEARCH.md §5](RESEARCH.md#5-mmds)).

## `intent=reboot`

The reset line reserves it (SPEC §8). It gets a producer when there is a
concrete in-guest reboot request to honour.

## A DAX lower plus overlay

An image on virtio-pmem mounted `ro,dax`, a writable upper on virtio-blk, and
overlayfs between them. fctl measured it to cut pinned host memory from
~448 MB to ~82 MB per VM at 16 VMs; it waits until real workloads show guest
page cache dominating.

It slots into the switch root without touching the initramfs boot: mount the
lower and the upper, assemble the overlay at `/newroot`, switch as now. What
vmsetup would then need:

- an `overlay_root` block in place of `root`: the lower `{device, fstype,
  options: "ro,dax"}`, the upper `{device, fstype}` or tmpfs, and `upperdir`
  and `workdir` *relative to the upper*, so they cannot name different
  filesystems, distinct and non-nested;
- **an assertion that `dax=always` and `ro` are in effect**, read from
  `/proc/self/mountinfo`, parking with `step=overlay` when missing. Losing DAX
  is silent: no error, only density collapsing at scale. And a write to a
  read-only pmem device on x86_64 is *discarded* by Firecracker rather than
  faulted.

The overlay exists because a writable DAX root is 19× slower on small-file
writes and fragments the image, so reads go to the DAX lower and writes to the
virtio-blk upper.

## Snapshots, and VMGenID

Restoring N VMs from one snapshot duplicates machine ids, entropy pools, RNG
seeds and pre-snapshot key material. Something resident has to react to a
VMGenID change by re-seeding and regenerating what is assumed unique. vmsetup
is never resident, so that would be zpinit's machine mode. Snapshots stay off
until that path exists.
