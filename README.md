# vminit

> Boot an OCI/Docker image as a Firecracker microVM.

`vminit` is a small static Go binary that runs as PID 1 inside a Firecracker
microVM. It performs the machine bringup a container runtime would normally do
for you — mounting filesystems, configuring the network from the inside,
setting the hostname, writing `/etc/resolv.conf` — and then hands off to
whatever supervisor the image is meant to run under.

**Status: planning.** No implementation yet. See [PLAN.md](PLAN.md) for the
full design and [docs/RESEARCH.md](docs/RESEARCH.md) for the findings it rests
on.

## Why this exists

A container gets an enormous amount for free from its runtime. The runtime
mounts `/proc`, `/sys` and `/dev`, creates a veth pair and configures it *from
the host*, writes `/etc/hosts` and `/etc/resolv.conf`, resolves the image's
`USER` to a uid, and applies `Entrypoint`, `Cmd`, `Env` and `WorkingDir`.

A microVM gets none of it. The kernel boots, mounts a root filesystem, and
executes one binary as PID 1. Everything else is that binary's problem —
including the network, because there is no shared namespace for the host to
reach into. That binary is `vminit`.

## Design in one line

```
kernel → vminit boot → [bringup] → execve(handoff target)
```

`vminit` does bringup and then replaces itself. It is deliberately **not** a
supervisor.

## Not tied to any one supervisor

One flat knob decides both what vminit does to the machine and whether it
survives the handoff — the two are not independent, so exposing them
separately would only invite combinations that cannot work:

```toml
[boot]
profile = "zpinit"   # zpinit | command | cloud-init | netplan | systemd-networkd | ifupdown
```

The rule behind the table: **`execve` only when the target requires PID 1;
otherwise fork.** zpinit requires it, systemd requires it, a bare command
does not.

| `profile` | config | handoff | PID 1 ends up as |
| --- | --- | --- | --- |
| `zpinit` | apply | `execve` | [zpinit](https://github.com/0ploy/zpinit) |
| `command` | apply | fork + supervise | **vminit** |
| `cloud-init` | generate | `execve` | the image's own init |
| `netplan` | generate | `execve` | the image's own init |
| `systemd-networkd` | generate | `execve` | the image's own init |
| `ifupdown` | generate | `execve` | the image's own init |

**Apply** profiles are for OCI images: vminit mounts, configures the network
via netlink, and hands off. **Generate** profiles are for full-distro images
you build yourself — systemd and OpenRC own mounts, cgroups and networking, so
vminit writes config files they will consume and touches nothing else.

Generate profiles are named by *mechanism*, not distro, because the distro does
not determine the answer: Ubuntu server uses netplan, Debian with systemd
usually uses raw `systemd-networkd`, Alpine uses ifupdown. If you are building
the image anyway, installing cloud-init and using `cloud-init` covers all of
them — see [PLAN.md §4.5](PLAN.md).

`vminit` and `zpinit` are designed to work well together, but neither requires
the other.

## Repository layout (planned)

| Path | Role |
| --- | --- |
| `cmd/vminit` | Guest-side binary: `boot` and `agent` subcommands |
| `cmd/vmpack` | Host-side packer: OCI image → bootable ext4 rootfs |
| `internal/config` | Shared config schema — the reason both binaries live here |

`vmpack` writes the config that `vminit` reads. Keeping them in one repo is
what stops the schema from drifting.

## License

MIT
