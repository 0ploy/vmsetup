# vmsetup

> Boot an OCI/Docker image as a Firecracker microVM.

`vmsetup` is a small static Go binary that runs first inside a Firecracker
microVM, as `/init` of a host-supplied initramfs. It mounts the VM's disk and
switches root into it, then performs the machine bringup a container runtime
would normally do for you: mounting filesystems, configuring the network from
the inside, setting the hostname, writing `/etc/resolv.conf`, resolving the
image's user. Then it **`execve`s [zpinit](https://github.com/0ploy/zpinit)**,
which is PID 1 for the rest of the VM's life.

vmsetup manages no process at any point. zpinit's *machine mode*
([zpinit#10](https://github.com/0ploy/zpinit/issues/10)) owns everything after
the `execve`: reaping, the graceful stop, ending the VM, `exec` over vsock and
rescue. Its host-side counterpart is [fctl](https://github.com/0ploy/fctl), the
VM manager that owns the Firecracker process, the kernel command line, the
image and the config.

```
CGO_ENABLED=0 go build ./cmd/vmsetup     # static, no libc
go run ./cmd/mkbundle -vmsetup vmsetup -zpinit zpinit -zpctl zpctl -o init.cpio
go test ./...                            # unit tests, no KVM needed
sudo KERNEL=/path/to/vmlinux ZPINIT=/path/to/zpinit test/run.sh   # boot matrix, needs KVM
```

See [SPEC.md](SPEC.md) for the spec,
[docs/RESEARCH.md](docs/RESEARCH.md) for the findings it rests on, and
[test/README.md](test/README.md) for the boot matrix.

## Why this exists

A container gets an enormous amount for free from its runtime. The runtime
mounts `/proc`, `/sys` and `/dev`, creates a veth pair and configures it *from
the host*, writes `/etc/hosts` and `/etc/resolv.conf`, resolves the image's
`USER` to a uid, and applies `Entrypoint`, `Cmd`, `Env` and `WorkingDir`.

A microVM gets none of it. The kernel boots and executes one binary as PID 1.
Everything else is that binary's problem, including the network, because
there is no shared namespace for the host to reach into. That binary is
`vmsetup`.

## Design in one line

```
kernel → initramfs /init = vmsetup (switch root, bringup, ~25 ms) → execve → zpinit (PID 1 for the life of the VM) → workload
```

This is Docker's runc/tini split, applied to a VM. vmsetup is the runc half:
it prepares the machine once and is gone. zpinit is the tini half: as the
kernel's own init it reaps, turns Ctrl+Alt+Del into a graceful stop, serves
`exec` over vsock, and ends the VM with sync and reboot, writing one line to
the serial port that tells the host why. Exactly one process manager runs per
VM.

zpinit detects that it is the kernel's init (PID 1 in the initial PID
namespace) rather than a container's, so machine mode needs no flag and can
never switch on inside a container.

The workload owns its disk but not PID 1: vmsetup and zpinit come from the
host on every boot, so nothing written to the disk can replace them.

**Security and isolation come first.** The workload is untrusted and the VM
boundary is the only boundary; vmsetup sits on the guest side of it and
enforces nothing itself.

## How the workload runs

zpinit is always the handoff target. The config says which of its modes:

```jsonc
"handoff": {
  "command":      ["/docker-entrypoint.sh", "nginx", "-g", "daemon off;"],  // absent → supervise mode
  "user":         "nginx",
  "working_dir":  "/",
  "stop_signal":  "SIGQUIT",
  "stop_timeout": "10s"
}
```

- **Plain Docker images** run in zpinit's wrap mode. zpinit forks the Docker
  command as its one child and stays PID 1, which is `docker run --init`.
- **zpinit-native images** run in supervise mode with their own
  `/etc/zpinit` services.

fctl composes the config per launch, fully resolved (there is nothing to merge
in the guest), and unknown fields are rejected.

## Repository layout

| Path | Role |
| --- | --- |
| `cmd/vmsetup` | The guest binary. As PID 1 it boots; by hand, `vmsetup check FILE` validates a config |
| `cmd/mkbundle` | Builds the init bundle: a reproducible cpio of vmsetup as `/init`, zpinit and zpctl |
| `internal/config` | Guest config schema and the blob format |
| `internal/mounts` | devtmpfs in the initramfs, the switch root, the mount table |
| `internal/machine` | What vmsetup hands zpinit: `machine.json` and the env file |
| `internal/bundle` | The cpio writer behind `mkbundle` |
| `internal/netconf` | Network configuration over netlink |
| `internal/identity` | Hostname, `/etc/hosts`, `/etc/resolv.conf` |
| `internal/userenv` | User resolution and env composition |
| `internal/vmlog` | The console log, and the park line the host matches |
| `test/` | Boot matrix: fixture builder, config-blob encoder, Firecracker harness, in-guest probe |

## License

MIT
