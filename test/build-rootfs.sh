#!/usr/bin/env bash
# Build the fixture for one boot-test scenario: the init bundle, a scratch-like
# ext4 image, the config blob fctl would compose for it, and scenario.env
# telling boot.sh what to expect.
#
# This stands in for fctl, not for its logic. The one thing it
# takes seriously is that the guest config, the probe's expectations and the
# host's TAP are all generated from the variables below, so they cannot drift.
#
# The image is deliberately `scratch`-like and carries no guest init: no
# shell, no libc, only the probe (the "workload") and zpinit's service config.
# vmsetup, zpinit and zpctl come from the init bundle, as under fctl. The
# image also carries a stub /usr/local/bin/zpinit that must never run.
#
# Scenarios (PLAN.md §11). Those marked [machine] need zpinit's machine mode
# (zpinit#10); run.sh skips them unless ZPINIT_MACHINE_MODE=1.
#   base         supervise mode: the probe is a zpinit service; Ctrl+Alt+Del
#                ends the VM.
#   routed32     base, on fctl's routed /32 with a link-local gateway (§5.3).
#   command      [machine] wrap mode: the probe is handoff.command, as an
#                unprivileged user; it exits on its own -> cause=exit.
#   stop         [machine] wrap mode, holding: Ctrl+Alt+Del -> stop_signal.
#   stopkill     [machine] stop, but the probe ignores SIGTERM -> SIGKILL.
#   park         a NIC MAC that does not exist: parks with step=network and
#                zpinit idles in rescue until Ctrl+Alt+Del.
#   noroot       the root device does not exist: parks with step=root, in the
#                initramfs.
#   nozpinit     a bundle without zpinit: parks with step=handoff and the VM
#                ends at once, with no kernel panic.
#   blob-magic   the config device has the wrong magic   -> parks, step=config
#   blob-version the config blob is version 2            -> parks, step=config
#   blob-noparam no vmsetup_config= on the command line   -> parks, step=config
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$DIR/.." && pwd)
# zpinit's source tree (github.com/0ploy/zpinit); the bundle is built from it.
ZPINIT=${ZPINIT:?set ZPINIT to a zpinit source checkout}
OUT=${OUT:-$DIR/rootfs.ext4}
BLOB=${BLOB:-$DIR/config.blob}
BUNDLE=${BUNDLE:-$DIR/init.cpio}
TREE=${TREE:-$DIR/rootfs}
SIZE=${SIZE:-192M}
SCENARIO=${SCENARIO:-base}

# ---- the machine ------------------------------------------------------------
HOST=vmsetup-test-01
GUEST_CIDR=172.16.0.2/30
GW=172.16.0.1
GUEST_MAC=06:00:AC:10:00:02   # what Firecracker gives the NIC
CONFIG_MAC=$GUEST_MAC         # what the config tells vmsetup to look for
HOST_CIDR=$GW/30              # what the host puts on the TAP
ROUTED=0                      # 1 => host also needs a /32 route to the guest
HTTP_PORT=${HTTP_PORT:-18080}
ROOT_DEVICE=/dev/vdb          # Firecracker names drives in PUT order: blob vda, root vdb

# ---- the handoff ------------------------------------------------------------
MODE=supervise                # supervise | wrap
PROBE_USER=root               # who the probe runs as
STOP_TIMEOUT=5s
PROBE_FLAGS=()
BUNDLE_FLAGS=()

# ---- what boot.sh expects ---------------------------------------------------
CMDLINE_PARAM=1               # pass vmsetup_config=
BLOB_FLAGS=()
WAIT_FOR='^PROBE-RESULT'      # serial line that means "ready for ACTION"
ACTION=cad                    # cad | none
EXPECT_PROBE=1                # the probe must report fail=0
STAYS_UP=0                    # 1 => the VM must still be up 2 s after WAIT_FOR
NEEDS_MACHINE_MODE=0          # 1 => only meaningful once zpinit#10 has landed
EXPECT_RESET=                 # ERE the final zpinit reset line must match, once zpinit#10 has landed
MIN_STOP_SECS=0               # CAD -> VM exit must take at least this long

case $SCENARIO in
  base)
    EXPECT_RESET='zpinit: reset intent=shutdown cause=stop status=0' ;;
  routed32)
    HOST=vmsetup-test-03
    GUEST_CIDR=10.77.0.5/32
    GW=169.254.0.1
    GUEST_MAC=06:00:0A:4D:00:05
    CONFIG_MAC=$GUEST_MAC
    HOST_CIDR=$GW/32
    ROUTED=1
    EXPECT_RESET='zpinit: reset intent=shutdown cause=stop status=0' ;;
  command)
    MODE=wrap; PROBE_USER=app; ACTION=none; NEEDS_MACHINE_MODE=1
    PROBE_FLAGS=(-session-leader)
    EXPECT_RESET='zpinit: reset intent=shutdown cause=exit status=0' ;;
  stop)
    MODE=wrap; PROBE_USER=app; NEEDS_MACHINE_MODE=1
    PROBE_FLAGS=(-session-leader -hold)
    EXPECT_RESET='zpinit: reset intent=shutdown cause=stop signal=SIGTERM' ;;
  stopkill)
    MODE=wrap; PROBE_USER=app; STOP_TIMEOUT=2s; MIN_STOP_SECS=2; NEEDS_MACHINE_MODE=1
    PROBE_FLAGS=(-session-leader -hold -ignore-term)
    EXPECT_RESET='zpinit: reset intent=shutdown cause=stop signal=SIGKILL' ;;
  park)
    CONFIG_MAC=06:00:DE:AD:BE:EF
    WAIT_FOR='^vmsetup: parked step=network '; EXPECT_PROBE=0; STAYS_UP=1
    EXPECT_RESET='zpinit: reset intent=shutdown cause=parked' ;;
  noroot)
    ROOT_DEVICE=/dev/vdz
    WAIT_FOR='^vmsetup: parked step=root error=".*vdz'; EXPECT_PROBE=0; STAYS_UP=1
    EXPECT_RESET='zpinit: reset intent=shutdown cause=parked' ;;
  nozpinit)
    BUNDLE_FLAGS=(-test-omit-zpinit)
    WAIT_FOR='^vmsetup: parked step=handoff error=".*no zpinit'; EXPECT_PROBE=0; ACTION=none ;;
  blob-magic)
    BLOB_FLAGS=(-magic EXT4)
    WAIT_FOR='^vmsetup: parked step=config error=".*not the config device'; EXPECT_PROBE=0; STAYS_UP=1
    EXPECT_RESET='zpinit: reset intent=shutdown cause=parked' ;;
  blob-version)
    BLOB_FLAGS=(-version 2)
    WAIT_FOR='^vmsetup: parked step=config error=".*version 2'; EXPECT_PROBE=0; STAYS_UP=1
    EXPECT_RESET='zpinit: reset intent=shutdown cause=parked' ;;
  blob-noparam)
    CMDLINE_PARAM=0
    WAIT_FOR='^vmsetup: parked step=config error="no vmsetup_config='; EXPECT_PROBE=0; STAYS_UP=1
    EXPECT_RESET='zpinit: reset intent=shutdown cause=parked' ;;
  *)
    echo "!! unknown scenario $SCENARIO" >&2; exit 2 ;;
esac

GUEST_IP=${GUEST_CIDR%%/*}
NAMESERVER=$GW
case $PROBE_USER in
  root) EXPECT_UID=0;    EXPECT_GID=0;    EXPECT_HOME=/root ;;
  app)  EXPECT_UID=1000; EXPECT_GID=1000; EXPECT_HOME=/home/app ;;
esac

PROBE_ARGV=(
  /usr/local/bin/vmprobe
  -hostname "$HOST"
  -interface eth0 -address "$GUEST_CIDR" -gateway "$GW"
  -nameserver "$NAMESERVER"
  -uid "$EXPECT_UID" -gid "$EXPECT_GID"
  -env IMAGE_VAR=from-image
  -env DEPLOY_ENV=prod
  -env LAST_WINS=second
  -env "HOME=$EXPECT_HOME"
  -envfile /run/zpinit/env
  -chain zpinit
  -serve ":$HTTP_PORT"
)
if [ "$MODE" = supervise ]; then
  PROBE_ARGV+=(-hold)
else
  PROBE_ARGV+=("${PROBE_FLAGS[@]}")
fi

# json_list ARG... -> ["ARG", ...]. Fixture values contain no quotes or
# backslashes, so no escaping is needed.
json_list() { local IFS=,; local q=(); for a in "$@"; do q+=("\"$a\""); done; echo "[${q[*]}]"; }

export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

echo "==> building static binaries"
# CGO_ENABLED=0 also makes os/user parse /etc/passwd directly instead of going
# through NSS, which is the behaviour PLAN.md §5.5 specifies.
( cd "$REPO" && go build -trimpath -ldflags "-s -w -X main.version=test" -o "$DIR/bin/vmsetup" ./cmd/vmsetup )
( cd "$REPO" && go build -trimpath -o "$DIR/bin/mkbundle" ./cmd/mkbundle )
( cd "$REPO" && go build -trimpath -ldflags "-s -w" -o "$DIR/bin/vmprobe" ./test/probe )
( cd "$REPO" && go build -trimpath -o "$DIR/bin/mkblob" ./test/mkblob )
( cd "$ZPINIT" && go build -trimpath -ldflags "-s -w" -o "$DIR/bin/zpinit" ./cmd/zpinit )
( cd "$ZPINIT" && go build -trimpath -ldflags "-s -w" -o "$DIR/bin/zpctl" ./cmd/zpctl )

for b in vmsetup vmprobe zpinit zpctl; do
  if ldd "$DIR/bin/$b" 2>&1 | grep -qv 'not a dynamic executable'; then
    echo "!! $b is dynamically linked; it will not run from an initramfs or a scratch rootfs" >&2
    exit 1
  fi
done

echo "==> building the init bundle $BUNDLE"
"$DIR/bin/mkbundle" -vmsetup "$DIR/bin/vmsetup" -zpinit "$DIR/bin/zpinit" -zpctl "$DIR/bin/zpctl" \
  "${BUNDLE_FLAGS[@]}" -o "$BUNDLE"

echo "==> assembling the image"
rm -rf "$TREE"
# No /dev, /proc, /sys or /run: vmsetup creates missing mount points on the
# writable root, and an OCI image need not have them.
install -d -m 0755 "$TREE"/{tmp,var,etc,root,usr/local/bin}
chmod 1777 "$TREE/tmp"

install -m 0755 "$DIR/bin/vmprobe" "$TREE/usr/local/bin/vmprobe"
# The image's own zpinit, as a zpinit-native image would carry. It is the
# probe binary, which refuses to run under that name: the bundle's zpinit is
# the only one that may ever be PID 1.
install -m 0755 "$DIR/bin/vmprobe" "$TREE/usr/local/bin/zpinit"

# The three files Docker's export leaves as 0-byte regular files.
: > "$TREE/etc/hostname"
: > "$TREE/etc/hosts"
: > "$TREE/etc/resolv.conf"

# An unknown user *name* parks by design, so resolving a name proves the
# lookup path rather than the numeric shortcut.
cat > "$TREE/etc/passwd" <<'EOF'
root:x:0:0:root:/root:/sbin/nologin
app:x:1000:1000:app:/home/app:/sbin/nologin
EOF
cat > "$TREE/etc/group" <<'EOF'
root:x:0:
tty:x:5:
app:x:1000:
EOF
install -d -m 0755 -o 1000 -g 1000 "$TREE/home/app"

# The image's zpinit config, as a zpinit-native image carries it. Supervise
# mode runs it; wrap mode ignores the services.
install -d -m 0755 "$TREE/etc/zpinit/services" "$TREE/etc/zpinit/entrypoint.d"
cat > "$TREE/etc/zpinit/zpinit.toml" <<'EOF'
default_stop_signal = "SIGTERM"
default_stop_timeout = "3s"
EOF
{
  echo 'name = "probe"'
  echo 'command = ['
  printf '  "%s",\n' "${PROBE_ARGV[@]}"
  echo ']'
  echo 'cwd = "/"'
  echo "user = \"$PROBE_USER\""
  echo 'restart = "never"'
} > "$TREE/etc/zpinit/services/10_probe.toml"

echo "==> building $OUT ($SIZE)"
rm -f "$OUT"
truncate -s "$SIZE" "$OUT"
mkfs.ext4 -q -F -L vmsetup-test -d "$TREE" "$OUT"

echo "==> writing the config blob"
# What fctl composes per launch: everything resolved, nothing left to merge.
# LAST_WINS appears twice to pin that a later entry wins.
if [ "$MODE" = wrap ]; then
  HANDOFF="\"command\": $(json_list "${PROBE_ARGV[@]}"),
    \"user\": \"$PROBE_USER\",
    \"working_dir\": \"/\",
    \"stop_signal\": \"SIGTERM\",
    \"stop_timeout\": \"$STOP_TIMEOUT\""
else
  HANDOFF="\"stop_signal\": \"SIGTERM\",
    \"stop_timeout\": \"$STOP_TIMEOUT\""
fi
cat > "$DIR/config.json" <<EOF
{
  "hostname": "$HOST",
  "handoff": {
    $HANDOFF
  },
  "env": [
    "PATH=/usr/local/bin:/usr/bin:/bin:/sbin",
    "IMAGE_VAR=from-image",
    "LAST_WINS=first",
    "DEPLOY_ENV=prod",
    "LAST_WINS=second"
  ],
  "network": {
    "interfaces": [
      { "name": "eth0", "mac": "$CONFIG_MAC", "address": "$GUEST_CIDR", "gateway": "$GW", "mmds": true }
    ],
    "nameservers": ["$NAMESERVER"],
    "extra_hosts": [{ "ip": "10.0.0.5", "host": "db.internal" }]
  },
  "root": { "device": "$ROOT_DEVICE", "fstype": "ext4" },
  "logging": { "target": "serial" }
}
EOF
"$DIR/bin/mkblob" "${BLOB_FLAGS[@]}" "$DIR/config.json" "$BLOB"

cat > "$DIR/scenario.env" <<EOF
# generated by build-rootfs.sh, SCENARIO=$SCENARIO -- do not edit
SCENARIO=$SCENARIO
GUEST_IP=$GUEST_IP
GUEST_CIDR=$GUEST_CIDR
GUEST_MAC=$GUEST_MAC
GW=$GW
HOST_CIDR=$HOST_CIDR
ROUTED=$ROUTED
HTTP_PORT=$HTTP_PORT
CMDLINE_PARAM=$CMDLINE_PARAM
WAIT_FOR='$WAIT_FOR'
ACTION=$ACTION
EXPECT_PROBE=$EXPECT_PROBE
STAYS_UP=$STAYS_UP
NEEDS_MACHINE_MODE=$NEEDS_MACHINE_MODE
EXPECT_RESET='$EXPECT_RESET'
MIN_STOP_SECS=$MIN_STOP_SECS
EOF

echo
echo "scenario: $SCENARIO ($MODE mode)"
echo "bundle:   $BUNDLE"
echo "image:    $OUT"
echo "config:   $BLOB (root $ROOT_DEVICE)"
echo "guest:    $GUEST_CIDR via $GW (mac $GUEST_MAC, config says $CONFIG_MAC)"
