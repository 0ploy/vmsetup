#!/usr/bin/env bash
# Boot the fixture rootfs under Firecracker and check what vminit did.
#
# Measured from the host, never with in-guest tooling: the guest clock is not
# trustworthy during early boot. The only thing read out of the guest is the
# probe's own pass/fail lines, which are statements about state rather than
# about time.
#
# Two boot-arg choices are deliberate and are themselves under test:
#   no console=  -- production configuration. vminit opens /dev/ttyS0 itself,
#                   and the port is enumerated on x86_64 either way. Set
#                   CONSOLE=1 to add it back when debugging a kernel-side
#                   problem.
#   no ip=       -- so the address and route can only come from vminit's
#                   netlink code. With ip= the network would work even if
#                   PLAN.md §5.3 were broken.
#   no rw        -- the kernel then mounts / read-only, which exercises the
#                   remount in mounts.RemountRootRW.
set -uo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
FCTL=${FCTL:-$(cd "$DIR/../../fctl" && pwd)}
KERNEL=${KERNEL:-$FCTL/test/vmlinux}
ROOTFS=${ROOTFS:-$DIR/rootfs.ext4}
SERIAL=${SERIAL:-$DIR/serial.log}
TAP=${TAP:-tap0}
GW=172.16.0.1
GUEST_IP=172.16.0.2
GUEST_MAC=06:00:AC:10:00:02
HOST_CIDR=$GW/30
ROUTED=0
HTTP_PORT=${HTTP_PORT:-18080}
TIMEOUT=${TIMEOUT:-40}

# build-rootfs.sh writes the host half of the network parameters it baked into
# the rootfs. Sourcing them is what keeps the TAP and the guest's expectations
# from drifting apart; the defaults above only matter when boot.sh is run on
# its own against an already-built rootfs.
[ -f "$DIR/net.env" ] && . "$DIR/net.env"

for f in "$KERNEL" "$ROOTFS"; do
  [ -f "$f" ] || { echo "!! missing $f" >&2; exit 1; }
done

cleanup() {
  [ -n "${FC_PID:-}" ] && kill "$FC_PID" 2>/dev/null
  [ -n "${HTTP_PID:-}" ] && kill "$HTTP_PID" 2>/dev/null
  [ "$ROUTED" = 1 ] && ip route del "$GUEST_IP/32" dev "$TAP" 2>/dev/null
  wait 2>/dev/null
}
trap cleanup EXIT

echo "==> host network: $TAP $HOST_CIDR"
ip link show "$TAP" >/dev/null 2>&1 || ip tuntap add dev "$TAP" mode tap
ip addr flush dev "$TAP"
ip addr replace "$HOST_CIDR" dev "$TAP"
ip link set "$TAP" up
if [ "$ROUTED" = 1 ]; then
  # The routed model: the guest's address is not in any prefix the host holds
  # on this link, so reaching it needs an explicit route. The gateway address
  # on the TAP is deliberately the same one every other TAP would carry.
  echo "==> routed: $GUEST_IP/32 dev $TAP"
  ip route replace "$GUEST_IP/32" dev "$TAP"
fi

echo "==> host listener on $GW:$HTTP_PORT"
# What the guest fetches to prove the network works end to end. Bound to the
# TAP address only, so a pass cannot come from anywhere else.
python3 - "$GW" "$HTTP_PORT" <<'PY' &
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Length", "5")
        self.end_headers()
        self.wfile.write(b"pong\n")
    def log_message(self, *a):
        pass
HTTPServer((sys.argv[1], int(sys.argv[2])), H).serve_forever()
PY
HTTP_PID=$!

CONFIG=$(mktemp)
BOOT_ARGS="reboot=k panic=1 pci=off init=/sbin/vminit"
[ -n "${CONSOLE:-}" ] && BOOT_ARGS="console=ttyS0 $BOOT_ARGS"
cat > "$CONFIG" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL",
    "boot_args": "$BOOT_ARGS"
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": "$ROOTFS",
      "is_root_device": true,
      "is_read_only": false
    }
  ],
  "machine-config": { "vcpu_count": 2, "mem_size_mib": 512 },
  "network-interfaces": [
    { "iface_id": "eth0", "guest_mac": "$GUEST_MAC", "host_dev_name": "$TAP" }
  ]
}
EOF

echo "==> booting (serial -> $SERIAL)"
: > "$SERIAL"
START=$(date +%s.%N)
# Firecracker forwards the guest's 8250 output to its own stdout, so no
# PUT /serial is needed for a --no-api boot.
firecracker --no-api --config-file "$CONFIG" > "$SERIAL" 2>&1 &
FC_PID=$!

# Wait for the probe's summary line rather than a fixed sleep, so the elapsed
# time below means something.
for _ in $(seq $((TIMEOUT * 10))); do
  grep -q '^PROBE-RESULT' "$SERIAL" 2>/dev/null && break
  kill -0 "$FC_PID" 2>/dev/null || break
  sleep 0.1
done
ELAPSED=$(echo "$(date +%s.%N) - $START" | bc)
rm -f "$CONFIG"

echo
echo "==== serial log ===="
cat "$SERIAL"
echo "===================="
echo

RESULT=$(grep '^PROBE-RESULT' "$SERIAL" | tail -1)
if [ -z "$RESULT" ]; then
  echo "!! the probe never reported. vminit did not reach the handoff." >&2
  exit 1
fi

FAILED=$(sed -n 's/.*fail=\([0-9]*\).*/\1/p' <<<"$RESULT")
printf 'host-measured wall clock to probe report: %.2fs\n' "$ELAPSED"
grep -c '^PROBE FAIL' "$SERIAL" >/dev/null
if [ "$FAILED" != "0" ]; then
  echo "!! $FAILED check(s) failed:" >&2
  grep '^PROBE FAIL' "$SERIAL" >&2
  exit 1
fi
echo "PASS: $RESULT"
