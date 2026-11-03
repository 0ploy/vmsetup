#!/usr/bin/env bash
# Run the boot matrix: build a fixture per scenario, boot it under Firecracker,
# and judge it from the host. Scenarios are listed in build-rootfs.sh.
#
# Needs a Linux host with KVM, firecracker on PATH, mkfs.ext4, curl, and root
# (for the TAP device and mkfs). Two inputs come from outside this repo:
#   KERNEL  a guest vmlinux with CONFIG_BLK_DEV_INITRD (fctl builds these)
#   ZPINIT  a zpinit source checkout (github.com/0ploy/zpinit)
#
#   sudo KERNEL=/path/to/vmlinux ZPINIT=/path/to/zpinit test/run.sh [scenario...]
set -uo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
: "${KERNEL:?set KERNEL to a guest vmlinux with CONFIG_BLK_DEV_INITRD}"
: "${ZPINIT:?set ZPINIT to a zpinit source checkout}"
export KERNEL ZPINIT
requested=${*:-base routed32 command stop stopkill park noroot nozpinit blob}
SCENARIOS=
for s in $requested; do
  # "blob" is shorthand for its three negative cases.
  [ "$s" = blob ] && s="blob-magic blob-version blob-noparam"
  SCENARIOS="$SCENARIOS $s"
done
fails=0

skipped=
for scenario in $SCENARIOS; do
  echo
  echo "############ scenario: $scenario"
  if ! SCENARIO="$scenario" "$DIR/build-rootfs.sh" > "$DIR/build-$scenario.log" 2>&1; then
    echo "!! build failed; see $DIR/build-$scenario.log" >&2
    tail -20 "$DIR/build-$scenario.log" >&2
    fails=$((fails + 1))
    continue
  fi
  if grep -q '^NEEDS_MACHINE_MODE=1' "$DIR/scenario.env" && [ "${ZPINIT_MACHINE_MODE:-0}" != 1 ]; then
    echo "-- skipped: needs zpinit's machine mode (zpinit#10); ZPINIT_MACHINE_MODE=1 runs it"
    skipped="$skipped $scenario"
    continue
  fi
  out=$(SERIAL="$DIR/serial-$scenario.log" "$DIR/boot.sh" 2>&1)
  status=$?
  grep -E '^(PROBE (FAIL|info)|PROBE-RESULT|PASS|!!|host-measured|reset line:|note:|vmsetup: parked)' <<<"$out"
  if [ "$status" -ne 0 ]; then
    echo "   full serial log: $DIR/serial-$scenario.log"
    fails=$((fails + 1))
  fi
done

echo
if [ "$fails" -ne 0 ]; then
  echo "FAIL: $fails scenario(s) failed"
  exit 1
fi
echo "OK: all scenarios passed${skipped:+ (skipped, need zpinit machine mode:$skipped)}"
