# vmsetup — archived

vmsetup was merged into **fctl** (`github.com/0ploy/fctl`) on 2026-11-04,
with its history. It had no user outside fctl: the kernel command line, the
config blob, the disk layout, the init bundle and the serial lines it relies on
are all fctl's design, so it now lives there as the guest half of fctl —
`cmd/vmsetup`, `internal/guest/`, and the spec in `docs/guest-spec.md`.

fctl's goal: **run every Docker image as a Firecracker microVM.**

This repository is read-only. The last standalone version is the parent of
this commit.
