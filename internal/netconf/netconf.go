// Package netconf configures the guest's network over netlink. See SPEC.md §5.
//
// A microVM has no shared namespace for the host to reach into, so unlike a
// container nobody configures this from the outside: the address, the route
// and the link state all have to be set from in here. That is done over
// netlink rather than by running `ip`, because a `scratch` image has no
// iproute2 and Alpine's busybox `ip` is not Debian's.
//
// Every operation is written to be idempotent, so re-applying a configuration
// is not an error.
package netconf

import (
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/0ploy/vmsetup/internal/config"
	"github.com/0ploy/vmsetup/internal/devwait"
)

// Logger is the subset of vmlog.Logger this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Apply brings up loopback and every configured interface.
func Apply(log Logger, nc config.Network) error {
	if err := upLoopback(); err != nil {
		return err
	}
	for _, iface := range nc.Interfaces {
		if err := applyInterface(log, iface); err != nil {
			return fmt.Errorf("interface %s: %w", iface.Name, err)
		}
	}
	return nil
}

// upLoopback brings lo up. Skipping it breaks a surprising amount of
// software, most of which never says why.
func upLoopback() error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find loopback: %w", err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return fmt.Errorf("bring loopback up: %w", err)
	}
	return nil
}

func applyInterface(log Logger, iface config.Interface) error {
	// Like the block devices, the NIC may still be probing when /init runs.
	var link netlink.Link
	err := devwait.Poll(devwait.Timeout, func() (err error) {
		link, err = findLink(iface)
		return err
	})
	if err != nil {
		return err
	}
	name := link.Attrs().Name

	// Before the link goes up. Firecracker can also advertise the MTU to the
	// driver (VIRTIO_NET_F_MTU), but the config is what vmsetup trusts.
	if iface.MTU != 0 && link.Attrs().MTU != iface.MTU {
		if err := netlink.LinkSetMTU(link, iface.MTU); err != nil {
			return fmt.Errorf("set mtu %d on %s: %w", iface.MTU, name, err)
		}
		log.Infof("mtu %d on %s", iface.MTU, name)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring %s up: %w", name, err)
	}

	if iface.Address != "" {
		addr, err := netlink.ParseAddr(iface.Address)
		if err != nil {
			return fmt.Errorf("parse address %q: %w", iface.Address, err)
		}
		if err := netlink.AddrReplace(link, addr); err != nil {
			return fmt.Errorf("add %s to %s: %w", iface.Address, name, err)
		}
		log.Infof("configured %s with %s", name, iface.Address)
	}

	if iface.Gateway != "" {
		gw := net.ParseIP(iface.Gateway)
		if gw == nil {
			return fmt.Errorf("parse gateway %q", iface.Gateway)
		}
		route := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Gw:        gw,
		}
		onlink := gatewayNeedsOnlink(iface.Address, gw)
		if onlink {
			route.Flags = int(netlink.FLAG_ONLINK)
		}
		if err := replaceRoute(route); err != nil {
			return fmt.Errorf("add default route via %s on %s: %w", iface.Gateway, name, err)
		}
		if onlink {
			log.Infof("default route via %s on %s (onlink)", iface.Gateway, name)
		} else {
			log.Infof("default route via %s on %s", iface.Gateway, name)
		}
	}

	if iface.MMDS {
		if err := addMMDSRoute(link, config.MMDSAddress); err != nil {
			return fmt.Errorf("add MMDS route on %s: %w", name, err)
		}
		log.Infof("MMDS route to %s on %s", config.MMDSAddress, name)
	}

	return nil
}

// findLink resolves the configured interface to a link, by MAC when one is
// given and by name otherwise.
//
// Pinning by MAC matters with more than one NIC: Firecracker does not
// guarantee that guest enumeration order matches the order the API was called
// in, so "eth0" is not reliably the interface fctl thinks it is. When both are
// given the MAC wins, and the link is renamed to the configured name so that
// anything downstream referring to eth0 by name still finds it.
func findLink(iface config.Interface) (netlink.Link, error) {
	if iface.MAC == "" {
		link, err := netlink.LinkByName(iface.Name)
		if err != nil {
			return nil, fmt.Errorf("find link %q: %w", iface.Name, err)
		}
		return link, nil
	}

	want, err := net.ParseMAC(iface.MAC)
	if err != nil {
		return nil, fmt.Errorf("parse mac %q: %w", iface.MAC, err)
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	for _, link := range links {
		if link.Attrs().HardwareAddr.String() != want.String() {
			continue
		}
		if link.Attrs().Name == iface.Name {
			return link, nil
		}
		// Rename to the configured name. Requires the link to be down,
		// which it is before LinkSetUp. A failure here is not fatal: the
		// address still lands on the right NIC, which is the point of
		// pinning by MAC.
		if err := netlink.LinkSetName(link, iface.Name); err != nil {
			return link, nil
		}
		return netlink.LinkByName(iface.Name)
	}
	return nil, fmt.Errorf("no link with mac %s", iface.MAC)
}

// gatewayNeedsOnlink reports whether the default route has to carry
// RTNH_F_ONLINK, given the address this interface was configured with.
//
// fctl allocates each VM a single routed address with **no on-link subnet at
// all** -- a bare /32 whose gateway is a link-local address shared by every
// tap on the host. The gateway is then in no connected prefix, and a plain
// route add fails with ENETUNREACH ("nexthop has invalid gateway"). ONLINK
// tells the kernel to treat it as directly reachable, which it genuinely is:
// the tap is point-to-point with the host on the other end. Verified end to
// end by the routed32 boot scenario (SPEC.md §5; docs/RESEARCH.md §8).
//
// The flag is set only when the configured address really does not cover the
// gateway. On a conventional subnet a mistyped gateway still fails loudly at
// route-add time (principle 5) rather than installing a route that silently
// blackholes -- which is exactly what setting ONLINK unconditionally would do.
//
// An unparseable or absent address yields false, leaving the kernel to produce
// its own error. That is the honest answer: with no address there is nothing
// to judge on-link-ness against, and a default route cannot work anyway.
func gatewayNeedsOnlink(address string, gw net.IP) bool {
	if address == "" {
		return false
	}
	_, prefix, err := net.ParseCIDR(address)
	if err != nil {
		return false
	}
	return !prefix.Contains(gw)
}

// addMMDSRoute adds a host route to the MMDS address over this link.
//
// MMDS lives at a link-local address with no gateway, so this is an on-link
// /32 with scope link. One NIC is enough: Firecracker's own documentation
// attaches MMDS to eth0, and a dedicated interface for it buys nothing.
func addMMDSRoute(link netlink.Link, addr string) error {
	ip := net.ParseIP(addr)
	if ip == nil {
		return fmt.Errorf("parse mmds address %q", addr)
	}
	v4 := ip.To4()
	if v4 == nil {
		return fmt.Errorf("mmds address %q is not IPv4", addr)
	}
	return replaceRoute(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)},
		Scope:     netlink.SCOPE_LINK,
	})
}

// replaceRoute adds a route, replacing one that already matches. RouteReplace
// is used rather than RouteAdd so re-applying a configuration is not an
// error.
func replaceRoute(route *netlink.Route) error {
	if err := netlink.RouteReplace(route); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil
		}
		return err
	}
	return nil
}
