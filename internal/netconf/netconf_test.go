package netconf

import (
	"net"
	"testing"
)

// The /32 cases are the ones that matter: they are fctl's routed-address
// model, where the gateway is a link-local address that belongs to no prefix
// the guest holds. Without ONLINK the route add fails and the VM boots with no
// default route at all. See the comment on gatewayNeedsOnlink.
func TestGatewayNeedsOnlink(t *testing.T) {
	cases := []struct {
		name    string
		address string
		gateway string
		want    bool
	}{
		{"routed /32, link-local gateway", "10.77.0.5/32", "169.254.0.1", true},
		{"routed /32, gateway elsewhere", "203.0.113.17/32", "192.0.2.1", true},
		{"conventional /30", "172.16.0.2/30", "172.16.0.1", false},
		{"conventional /24", "10.0.0.50/24", "10.0.0.1", false},
		{"/31 point to point", "10.0.0.0/31", "10.0.0.1", false},
		{"gateway outside a /24 still needs it", "10.0.0.50/24", "10.9.9.1", true},
		{"gateway is the address itself", "10.0.0.1/24", "10.0.0.1", false},

		// Degenerate input must not claim on-link-ness it cannot establish.
		// Returning false leaves the kernel to produce its own error rather
		// than installing a route that would blackhole.
		{"no address", "", "169.254.0.1", false},
		{"unparseable address", "not-a-cidr", "169.254.0.1", false},
		{"bare address, no prefix", "10.77.0.5", "169.254.0.1", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := net.ParseIP(tc.gateway)
			if gw == nil {
				t.Fatalf("bad test gateway %q", tc.gateway)
			}
			if got := gatewayNeedsOnlink(tc.address, gw); got != tc.want {
				t.Errorf("gatewayNeedsOnlink(%q, %s) = %v, want %v",
					tc.address, tc.gateway, got, tc.want)
			}
		})
	}
}

// A v6 gateway must not be reported on-link against a v4 prefix just because
// Contains returns false for the wrong reason. This is a guard for the day
// IPv6 arrives, not a claim that it works today.
func TestGatewayNeedsOnlinkMixedFamily(t *testing.T) {
	if !gatewayNeedsOnlink("10.0.0.50/24", net.ParseIP("fe80::1")) {
		t.Error("a v6 gateway is not inside a v4 prefix; expected onlink")
	}
	if gatewayNeedsOnlink("fd00::5/64", net.ParseIP("fd00::1")) {
		t.Error("v6 gateway inside its own v6 prefix should not need onlink")
	}
}
