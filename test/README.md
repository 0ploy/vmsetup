# Boot tests

```
go test ./...                     # unit tests, no KVM needed
sudo KERNEL=/path/to/vmlinux ZPINIT=/path/to/zpinit test/run.sh [scenario...]
```

`run.sh` builds a fixture per scenario and boots it under Firecracker the way
fctl does: the init bundle as `initrd_path`, no root device, the config blob as
`/dev/vda` and the image as `/dev/vdb`. It judges the result **from the host**,
never with in-guest timing. It waits for a scenario-specific serial line,
performs the scenario's action (Ctrl+Alt+Del, resent while the VM is up, or
nothing), waits for the VM to end, then checks the probe's report, the park and
reset lines, and that the kernel did not panic.

`build-rootfs.sh` generates the bundle, the image, the config (encoded by
`mkblob` with vmsetup's own encoder after validating it with vmsetup's own
parser), the probe's expectations and the TAP parameters from one set of shell
variables, so they cannot drift.

The image is `scratch`-like: static Go binaries, no shell, no libc. It carries
a stub `/usr/local/bin/zpinit` that fails the run if it is ever executed,
standing in for a zpinit-native image's own copy.

The network check is host-initiated: the probe listens and the host fetches
`/ping` from the guest's address. A host firewall dropping guest-initiated
connections cannot fail it, and the reply still has to leave over the guest's
address and routes; under `routed32` it can only leave via the onlink default
route.

| Scenario | Proves |
| --- | --- |
| `base` | supervise mode: the probe, run as a zpinit service, checks the root is the writable disk, `/run/zpinit/bin` is a read-only tmpfs, `/proc/1/exe` is the bundle's zpinit, the initramfs was freed, `machine.json` and `/run/zpinit/env` are mode 0600, the mount table and its flags, network, identity, env, user, the process chain `probe → zpinit(1)` and the initial PID namespace |
| `routed32` | `base` on a routed `/32` with the onlink default route |
| `park` | a MAC no NIC has: `parked step=network`, zpinit idles in rescue, the VM stays up until stopped |
| `noroot` | a root device that does not exist: `parked step=root`, zpinit idles in the initramfs |
| `nozpinit` | a bundle without zpinit: `parked step=handoff`, then the VM ends at once, with no panic and no reset line from vmsetup |
| `blob-magic`, `blob-version`, `blob-noparam` | a wrong magic, a version-2 header and a missing `vmsetup_config=` each park with `step=config` |
| `command` | wrap mode: the probe as `handoff.command`, as uid 1000, child of zpinit(1); it exits → `cause=exit status=0` |
| `stop`, `stopkill` | wrap-mode stop: `stop_signal`, then `SIGKILL` at `stop_timeout` |

`command`, `stop` and `stopkill`, and the reset line in every scenario, need
zpinit's machine mode ([zpinit#10](https://github.com/0ploy/zpinit/issues/10)).
`run.sh` skips them until `ZPINIT_MACHINE_MODE=1` is set.
