#!/usr/bin/env bash
# Assemble a bootable ext4 rootfs for the zpinit-handoff boot test.
#
# This is a stand-in for vmpack, not a replacement for it: it hand-writes the
# config that vmpack will eventually generate from an OCI image config, so the
# guest half can be tested before the packer exists. The one thing it does take
# seriously is that the vminit config and the probe's expectations are
# generated from the same variables below, so the fixture cannot drift.
#
# The result is a `scratch`-like image: no shell, no libc, no userspace at all
# beyond three static Go binaries. That is deliberate. It is the case that
# proves vminit has no dependency on the guest image.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$DIR/.." && pwd)
ZPINIT=${ZPINIT:-$(cd "$REPO/../zpinit" && pwd)}
OUT=${OUT:-$DIR/rootfs.ext4}
TREE=${TREE:-$DIR/rootfs}
SIZE=${SIZE:-192M}

# The machine the guest is expected to end up with. Both the vminit config and
# the probe's command line are generated from these.
#
# SCENARIO=base     one layer: /etc/vminit/config.json only.
# SCENARIO=override two layers, so the merge rules of PLAN.md §4.1 are
#                   exercised on a running machine and not only in unit tests.
# SCENARIO=routed32 fctl's routed-address model: a bare /32 with no on-link
#                   subnet at all, reached through a link-local gateway that
#                   is shared by every tap on the host. Exercises the ONLINK
#                   path in §5.3 -- without it the default route cannot be
#                   installed and the guest reaches nothing.
SCENARIO=${SCENARIO:-base}
# BASE_HOST goes in config.json; HOST is what the merged config must resolve
# to. They differ under SCENARIO=override so the override is provably applied.
BASE_HOST=vminit-test-01
HOST=$BASE_HOST
GUEST_CIDR=172.16.0.2/30
GW=172.16.0.1
GUEST_MAC=06:00:AC:10:00:02
HOST_CIDR=$GW/30      # what the host puts on the TAP
ROUTED=0              # 1 => host also needs an explicit /32 route to the guest
if [ "$SCENARIO" = routed32 ]; then
  BASE_HOST=vminit-test-03
  HOST=$BASE_HOST
  GUEST_CIDR=10.77.0.5/32
  GW=169.254.0.1
  GUEST_MAC=06:00:0A:4D:00:05
  HOST_CIDR=$GW/32
  ROUTED=1
fi
GUEST_IP=${GUEST_CIDR%%/*}
NAMESERVER=$GW
HTTP_PORT=${HTTP_PORT:-18080}

# What the merged configuration is expected to resolve to.
EXPECT_USER=root
EXPECT_UID=0
EXPECT_GID=0
EXPECT_HOME=/root
EXPECT_OVERRIDDEN=from-extra-env
EXPECT_EXTRA_ENV=(-env "IMAGE_VAR=from-image" -env "EXTRA_VAR=from-extra-env")
if [ "$SCENARIO" = override ]; then
  HOST=vminit-test-02
  EXPECT_USER=app
  EXPECT_UID=1000
  EXPECT_GID=1000
  EXPECT_HOME=/home/app
  EXPECT_OVERRIDDEN=from-override
  # IMAGE_VAR and EXTRA_VAR survive an override that mentions neither: that
  # is the per-variable half of the merge rule.
  EXPECT_EXTRA_ENV=(-env "IMAGE_VAR=from-image" -env "EXTRA_VAR=from-extra-env" -env "FROM_OVERRIDE=yes")
fi

export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

echo "==> building static binaries"
# CGO_ENABLED=0 is not just about portability here: it is what makes os/user
# parse /etc/passwd directly instead of going through NSS, which is the
# behaviour PLAN.md §5.5 specifies.
( cd "$REPO" && go build -trimpath -ldflags "-s -w -X main.version=test" -o "$DIR/bin/vminit" ./cmd/vminit )
( cd "$REPO" && go build -trimpath -ldflags "-s -w" -o "$DIR/bin/vmprobe" ./test/probe )
( cd "$ZPINIT" && go build -trimpath -ldflags "-s -w" -o "$DIR/bin/zpinit" ./cmd/zpinit )
( cd "$ZPINIT" && go build -trimpath -ldflags "-s -w" -o "$DIR/bin/zpctl" ./cmd/zpctl )

for b in vminit vmprobe zpinit zpctl; do
  if ldd "$DIR/bin/$b" 2>&1 | grep -qv 'not a dynamic executable'; then
    echo "!! $b is dynamically linked; it will not run in a scratch rootfs" >&2
    exit 1
  fi
done

echo "==> assembling tree"
rm -rf "$TREE"
# /proc, /sys, /dev, /run and /sys/fs/cgroup are mountpoints vminit creates if
# missing, but a real image has them, so the fixture does too. /tmp and /var
# are here because software expects them, not because vminit needs them.
install -d -m 0755 "$TREE"/{dev,proc,sys,run,tmp,var,etc,root,usr/local/bin,sbin}
chmod 1777 "$TREE/tmp"

install -m 0755 "$DIR/bin/vminit" "$TREE/sbin/vminit"
install -m 0755 "$DIR/bin/vmprobe" "$TREE/usr/local/bin/vmprobe"
install -m 0755 "$DIR/bin/zpinit" "$TREE/usr/local/bin/zpinit"
install -m 0755 "$DIR/bin/zpctl" "$TREE/usr/local/bin/zpctl"

# The three files Docker's export leaves as 0-byte regular files. Reproducing
# that exactly is the point: vminit has to fill them in.
: > "$TREE/etc/hostname"
: > "$TREE/etc/hosts"
: > "$TREE/etc/resolv.conf"

# A minimal passwd/group so user resolution has something to resolve. An
# unknown *name* is fatal by design, so a fixture that resolves a name proves
# the lookup path rather than the numeric shortcut.
cat > "$TREE/etc/passwd" <<'EOF'
root:x:0:0:root:/root:/sbin/nologin
app:x:1000:1000:app:/home/app:/sbin/nologin
EOF
cat > "$TREE/etc/group" <<'EOF'
root:x:0:
tty:x:5:
app:x:1000:
EOF
install -d -m 0755 "$TREE/home/app"

echo "==> writing /etc/vminit/config.json"
install -d -m 0755 "$TREE/etc/vminit"
cat > "$TREE/etc/vminit/config.json" <<EOF
{
  "hostname": "$BASE_HOST",
  "image": {
    "entrypoint": ["/usr/local/bin/vmprobe"],
    "cmd": [],
    "env": [
      "PATH=/usr/local/bin:/usr/bin:/bin:/sbin",
      "IMAGE_VAR=from-image",
      "OVERRIDDEN=from-image"
    ],
    "working_dir": "/",
    "user": "root",
    "stop_signal": "SIGTERM"
  },
  "extra_env": {
    "OVERRIDDEN": "from-extra-env",
    "EXTRA_VAR": "from-extra-env"
  },
  "network": {
    "interfaces": [
      {
        "name": "eth0",
        "mac": "$GUEST_MAC",
        "address": "$GUEST_CIDR",
        "gateway": "$GW",
        "mmds": true
      }
    ],
    "nameservers": ["$NAMESERVER"],
    "extra_hosts": [{ "ip": "10.0.0.5", "host": "db.internal" }]
  },
  "boot": {
    "profile": "zpinit",
    "path": "/usr/local/bin/zpinit",
    "args": []
  },
  "logging": { "target": "serial" },
  "mmds": { "enabled": false }
}
EOF
# boot.args is empty on purpose. zpinit has no --vm flag yet
# (ZPINIT_CHANGES.md §1), and older releases reject unknown flags, so passing
# it would fail the exec. vminit warns about this at boot; the warning showing
# up in the serial log is itself part of what this test checks.

# The per-launch layer fctl writes into the rootfs before boot. It restates
# hostname and adds two variables, and says nothing about the network or the
# image, which must therefore survive untouched.
if [ "$SCENARIO" = override ]; then
  echo "==> writing /etc/vminit/override.json"
  cat > "$TREE/etc/vminit/override.json" <<EOF
{
  "hostname": "$HOST",
  "user_override": "$EXPECT_USER",
  "extra_env": {
    "OVERRIDDEN": "$EXPECT_OVERRIDDEN",
    "FROM_OVERRIDE": "yes"
  }
}
EOF
fi

echo "==> writing zpinit config"
install -d -m 0755 "$TREE/etc/zpinit/services" "$TREE/etc/zpinit/entrypoint.d"
cat > "$TREE/etc/zpinit/zpinit.toml" <<'EOF'
# Deliberately minimal. Everything image-derived reaches zpinit through the
# service file and the environment vminit composes, never through OCI
# awareness in zpinit itself.
default_stop_signal = "SIGTERM"
default_stop_timeout = "5s"
EOF

# The service file vmpack will generate from entrypoint + cmd + user +
# workdir. -hold keeps the machine up after reporting: with no VM mode in
# zpinit yet, a service that exits takes PID 1 with it and panics the kernel.
cat > "$TREE/etc/zpinit/services/10_probe.toml" <<EOF
name = "probe"
command = [
  "/usr/local/bin/vmprobe",
  "-hostname", "$HOST",
  "-interface", "eth0",
  "-address", "$GUEST_CIDR",
  "-gateway", "$GW",
  "-nameserver", "$NAMESERVER",
  "-uid", "$EXPECT_UID",
  "-gid", "$EXPECT_GID",
$(printf '  "%s",\n' "${EXPECT_EXTRA_ENV[@]}")
  "-env", "OVERRIDDEN=$EXPECT_OVERRIDDEN",
  "-env", "HOME=$EXPECT_HOME",
  "-http", "http://$GW:$HTTP_PORT/ping",
  "-hold",
]
cwd = "/"
user = "$EXPECT_USER"
restart = "never"
EOF

echo "==> building $OUT ($SIZE)"
rm -f "$OUT"
truncate -s "$SIZE" "$OUT"
mkfs.ext4 -q -F -L vminit-test -d "$TREE" "$OUT"

# The host side of the same set of variables, so boot.sh configures the TAP to
# match the machine this rootfs expects instead of keeping its own copy. The
# fixture already generates the vminit config and the probe's expectations from
# one source; this extends that to the host, which is where the /32 scenario
# needs a different setup rather than merely a different number.
cat > "$DIR/net.env" <<EOF
# generated by build-rootfs.sh, SCENARIO=$SCENARIO -- do not edit
GUEST_IP=$GUEST_IP
GUEST_CIDR=$GUEST_CIDR
GUEST_MAC=$GUEST_MAC
GW=$GW
HOST_CIDR=$HOST_CIDR
ROUTED=$ROUTED
HTTP_PORT=$HTTP_PORT
EOF

echo
echo "rootfs:   $OUT"
echo "hostname: $HOST"
echo "guest:    $GUEST_CIDR via $GW (mac $GUEST_MAC)"
echo "http:     http://$GW:$HTTP_PORT/ping"
