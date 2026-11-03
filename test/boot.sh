#!/usr/bin/env bash
# Boot one built scenario under Firecracker and judge it from the host.
#
# Measured and judged from the host, never with in-guest tooling: the guest
# clock is not trustworthy during early boot. What is read out of the guest is
# the probe's pass/fail lines, vmsetup's park line and zpinit's reset line --
# statements about state, matched the way fctl will match them.
#
# The boot is fctl's production shape:
#   initrd_path  the init bundle; the kernel runs its /init, which is vmsetup
#   no root device, so Firecracker adds no root= and there is no init=
#   drives in PUT order: the config blob /dev/vda (read-only), the image /dev/vdb
#   no console=  production configuration; vmsetup opens /dev/ttyS0 itself.
#                CONSOLE=1 adds it back for kernel-side debugging.
#   no ip=       so the address and route can only come from vmsetup.
#
# zpinit's reset line exists only once zpinit has machine mode (zpinit#10).
# Until then the reset line is reported but not required; set
# ZPINIT_MACHINE_MODE=1 to require it.
set -uo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
# The guest kernel is fctl's (github.com/0ploy/fctl builds it); point at one.
KERNEL=${KERNEL:?set KERNEL to a guest vmlinux with CONFIG_BLK_DEV_INITRD (PLAN.md §7.1)}
ROOTFS=${ROOTFS:-$DIR/rootfs.ext4}
BLOB=${BLOB:-$DIR/config.blob}
BUNDLE=${BUNDLE:-$DIR/init.cpio}
MACHINE_MODE=${ZPINIT_MACHINE_MODE:-0}
SERIAL=${SERIAL:-$DIR/serial.log}
TAP=${TAP:-tap0}
TIMEOUT=${TIMEOUT:-40}
SOCK=$DIR/fc.sock

[ -f "$DIR/scenario.env" ] || { echo "!! run build-rootfs.sh first" >&2; exit 1; }
. "$DIR/scenario.env"

for f in "$KERNEL" "$ROOTFS" "$BLOB" "$BUNDLE"; do
  [ -f "$f" ] || { echo "!! missing $f" >&2; exit 1; }
done

cleanup() {
  [ -n "${FC_PID:-}" ] && kill "$FC_PID" 2>/dev/null
  [ -n "${PING_PID:-}" ] && kill "$PING_PID" 2>/dev/null
  [ "$ROUTED" = 1 ] && ip route del "$GUEST_IP/32" dev "$TAP" 2>/dev/null
  rm -f "$SOCK" "${CONFIG:-}"
  wait 2>/dev/null
}
trap cleanup EXIT

echo "==> host network: $TAP $HOST_CIDR"
ip link show "$TAP" >/dev/null 2>&1 || ip tuntap add dev "$TAP" mode tap
ip addr flush dev "$TAP"
ip addr replace "$HOST_CIDR" dev "$TAP"
ip link set "$TAP" up
if [ "$ROUTED" = 1 ]; then
  # The guest's address is in no prefix the host holds on this link.
  ip route replace "$GUEST_IP/32" dev "$TAP"
fi


BOOT_ARGS="reboot=k panic=1 pci=off"
[ "$CMDLINE_PARAM" = 1 ] && BOOT_ARGS="vmsetup_config=/dev/vda $BOOT_ARGS"
[ -n "${CONSOLE:-}" ] && BOOT_ARGS="console=ttyS0 $BOOT_ARGS"
CONFIG=$(mktemp)
cat > "$CONFIG" <<EOF
{
  "boot-source": { "kernel_image_path": "$KERNEL", "initrd_path": "$BUNDLE", "boot_args": "$BOOT_ARGS" },
  "drives": [
    { "drive_id": "config", "path_on_host": "$BLOB", "is_root_device": false, "is_read_only": true },
    { "drive_id": "rootfs", "path_on_host": "$ROOTFS", "is_root_device": false, "is_read_only": false }
  ],
  "machine-config": { "vcpu_count": 2, "mem_size_mib": 512 },
  "network-interfaces": [
    { "iface_id": "eth0", "guest_mac": "$GUEST_MAC", "host_dev_name": "$TAP" }
  ]
}
EOF

alive() { kill -0 "$FC_PID" 2>/dev/null; }
now() { date +%s.%N; }

echo "==> booting $SCENARIO (serial -> $SERIAL)"
rm -f "$SOCK"
: > "$SERIAL"
START=$(now)
firecracker --api-sock "$SOCK" --config-file "$CONFIG" > "$SERIAL" 2>&1 &
FC_PID=$!

reasons=()

# The probe's network proof is host-initiated (see awaitPing in probe/main.go):
# once it says it is serving, fetch /ping from the guest's address.
(
  for _ in $(seq $((TIMEOUT * 10))); do
    grep -q '^PROBE serving' "$SERIAL" 2>/dev/null && break
    sleep 0.1
  done
  for _ in $(seq 20); do
    curl -fsS --max-time 2 "http://$GUEST_IP:$HTTP_PORT/ping" >/dev/null 2>&1 && break
    sleep 0.25
  done
) &
PING_PID=$!

# 1. Wait for the scenario's trigger line.
triggered=0
for _ in $(seq $((TIMEOUT * 10))); do
  if grep -Eq "$WAIT_FOR" "$SERIAL" 2>/dev/null; then triggered=1; break; fi
  alive || break
  sleep 0.1
done
READY=$(now)
[ "$triggered" = 1 ] || reasons+=("never saw /$WAIT_FOR/")

# 2. Act.
STOP_AT=
if [ "$triggered" = 1 ] && [ "$STAYS_UP" = 1 ]; then
  # A parked VM must stay up under zpinit's rescue, not reset; give it time
  # to do the wrong thing.
  sleep 2
  alive || reasons+=("the VM did not stay up after parking")
fi
cad() {
  curl -fsS --unix-socket "$SOCK" -X PUT http://localhost/actions \
    -H 'Content-Type: application/json' -d '{"action_type":"SendCtrlAltDel"}'
}
CAD_SENT=0
if [ "$triggered" = 1 ] && [ "$ACTION" = cad ]; then
  STOP_AT=$(now)
  cad || reasons+=("SendCtrlAltDel failed")
  CAD_SENT=1
  # A Ctrl+Alt+Del that arrives before the guest's i8042/atkbd driver has
  # finished probing is lost: the initramfs boot reaches the probe's report
  # in ~0.4 s. Resend while the VM is still up, as fctl's stop has to.
  for _ in $(seq 6); do
    for _ in $(seq 10); do alive || break 2; sleep 0.1; done
    [ "$MIN_STOP_SECS" != 0 ] && break   # a slow stop is the point there
    cad >/dev/null 2>&1 && CAD_SENT=$((CAD_SENT + 1))
  done
  [ "$CAD_SENT" -gt 1 ] && echo "note: Ctrl+Alt+Del sent $CAD_SENT times before the VM ended"
fi

# 3. The VM has to end by itself: the reset sentinel, then reboot=k.
for _ in $(seq $((TIMEOUT * 10))); do
  alive || break
  sleep 0.1
done
END=$(now)
if alive; then
  reasons+=("Firecracker still running ${TIMEOUT}s later")
fi

echo
echo "==== serial log ===="
cat "$SERIAL"
echo "===================="
echo

# 4. Judge.
if grep -q 'Attempted to kill init\|Kernel panic' "$SERIAL"; then
  reasons+=("kernel panic")
fi
if grep -q 'SHADOW-STUB-RAN' "$SERIAL"; then
  reasons+=("the image's own /usr/local/bin/zpinit ran; only the bundle's may")
fi
if [ "$EXPECT_PROBE" = 1 ]; then
  RESULT=$(grep '^PROBE-RESULT' "$SERIAL" | tail -1)
  if [ -z "$RESULT" ]; then
    reasons+=("the probe never reported")
  elif ! grep -q 'fail=0' <<<"$RESULT"; then
    reasons+=("probe: $RESULT")
    grep '^PROBE FAIL' "$SERIAL" >&2
  fi
fi
# The tty's ONLCR turns \n into \r\n on the serial port; fctl must strip it too.
RESET=$(tr -d '\r' < "$SERIAL" | grep '^zpinit: reset ' | tail -1)
if [ "$MACHINE_MODE" = 1 ]; then
  if [ -z "$EXPECT_RESET" ]; then
    [ -n "$RESET" ] && reasons+=("unexpected reset line: $RESET")
  elif ! grep -Eqx "$EXPECT_RESET" <<<"$RESET"; then
    reasons+=("reset line ${RESET:-<none>}, want /$EXPECT_RESET/")
  fi
fi
if tr -d '\r' < "$SERIAL" | grep -q '^vmsetup: reset '; then
  reasons+=("vmsetup wrote a reset line; ending the VM is zpinit's")
fi
if [ -n "$STOP_AT" ] && [ "$MIN_STOP_SECS" != 0 ]; then
  took=$(echo "$END - $STOP_AT" | bc)
  if [ "$(echo "$took < $MIN_STOP_SECS" | bc)" = 1 ]; then
    reasons+=("stop took ${took}s, expected at least ${MIN_STOP_SECS}s")
  fi
  printf 'host-measured stop: %.2fs\n' "$took"
fi

printf 'host-measured to trigger: %.2fs, to VM exit: %.2fs\n' \
  "$(echo "$READY - $START" | bc)" "$(echo "$END - $START" | bc)"
if [ "$MACHINE_MODE" = 1 ]; then
  echo "reset line: ${RESET:-<none>}"
else
  echo "reset line: ${RESET:-<none>} (not required until zpinit#10; ZPINIT_MACHINE_MODE=1 requires /${EXPECT_RESET:-none}/)"
fi
if [ "${#reasons[@]}" -ne 0 ]; then
  for r in "${reasons[@]}"; do echo "!! $r" >&2; done
  exit 1
fi
echo "PASS: $SCENARIO"
