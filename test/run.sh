#!/usr/bin/env bash
# Run the boot matrix: build a fixture rootfs per scenario, boot it under
# Firecracker, and check what vminit did from inside the guest.
#
# Needs a Linux host with KVM, /usr/local/bin/firecracker, mkfs.ext4, and root
# (for the TAP device and mkfs). Everything else is built from this repo and
# ../zpinit.
set -uo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
SCENARIOS=${*:-base override routed32}
fails=0

for scenario in $SCENARIOS; do
  echo
  echo "############ scenario: $scenario"
  if ! SCENARIO="$scenario" "$DIR/build-rootfs.sh" > "$DIR/build-$scenario.log" 2>&1; then
    echo "!! build failed; see $DIR/build-$scenario.log" >&2
    tail -20 "$DIR/build-$scenario.log" >&2
    fails=$((fails + 1))
    continue
  fi
  out=$(SERIAL="$DIR/serial-$scenario.log" "$DIR/boot.sh" 2>&1)
  status=$?
  grep -E '^(PROBE (FAIL|info)|PROBE-RESULT|PASS|!!|host-measured)' <<<"$out"
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
echo "OK: all scenarios passed"
