package models

import (
	"fmt"
	"net/netip"
)

const (
	NetworkModeIsolated = "isolated"
	NetworkModeShared   = "shared"

	SharedVLANTagFirst = 347
	SharedVLANTagLast  = 355
	SharedStripeCount  = 9
	SharedStripeSlots  = 16
	SharedAddressCIDR  = "10.110.0.0/16"
	SharedRunnerSlots  = 14
)

// Stripe is one pre-provisioned Single VM network. The tag does not encode
// the address. Tags 347–355 map onto consecutive /26s in 10.110.0.0/16.
type Stripe struct {
	VLANTag     int
	CIDR        string
	Gateway     string
	PortGroup   string
	DHCPStart   string
	DHCPEnd     string
	RunnerFirst string
	RunnerLast  string
}

// SharedStripes returns the nine generic stripes in tag order.
func SharedStripes() []Stripe {
	out := make([]Stripe, 0, SharedStripeCount)
	for tag := SharedVLANTagFirst; tag <= SharedVLANTagLast; tag++ {
		out = append(out, stripeForTag(tag))
	}
	return out
}

func stripeForTag(tag int) Stripe {
	base := netip.MustParseAddr("10.110.0.0")
	base = addHosts(base, (tag-SharedVLANTagFirst)*64)
	return Stripe{
		VLANTag:     tag,
		CIDR:        base.String() + "/26",
		Gateway:     addHosts(base, 1).String() + "/26",
		PortGroup:   fmt.Sprintf("Shared-VLAN%d", tag),
		DHCPStart:   addHosts(base, 16).String(),
		DHCPEnd:     addHosts(base, 47).String(),
		RunnerFirst: addHosts(base, 2).String(),
		RunnerLast:  addHosts(base, 15).String(),
	}
}

// RunnerAddresses lists the fourteen runner addresses on this stripe, from
// offset .2 through .15. Student DHCP starts at .16.
func (s Stripe) RunnerAddresses() []string {
	first := netip.MustParseAddr(s.RunnerFirst)
	last := netip.MustParseAddr(s.RunnerLast)
	out := make([]string, 0, SharedRunnerSlots)
	for ip := first; ip.Compare(last) <= 0; ip = ip.Next() {
		out = append(out, ip.String())
	}
	return out
}

func addHosts(addr netip.Addr, n int) netip.Addr {
	b := addr.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += uint32(n)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// PodCreateNetwork names the port group a pod_create uses and says whether
// that job may create VLAN, DHCP, firewall, or portgroup objects. A shared
// pod only places a VM. An empty mode stays on the isolated path.
func PodCreateNetwork(mode string, vlanTag int) (pgName string, mutate bool) {
	if mode == NetworkModeShared {
		return fmt.Sprintf("Shared-VLAN%d", vlanTag), false
	}
	return fmt.Sprintf("Pod-VLAN%d", vlanTag), true
}

// PickStripe chooses the active stripe with the fewest non-destroyed pods,
// provided that count is under SharedStripeSlots. It does not look at a
// template. Ties break toward the lower tag. A stripe that is not active
// makes the whole set unusable. A full set returns ok=false and full=true.
func PickStripe(occupancy map[int]int, active map[int]bool) (Stripe, bool, bool) {
	stripes := SharedStripes()
	for _, stripe := range stripes {
		if !active[stripe.VLANTag] {
			return Stripe{}, false, false
		}
	}
	best := -1
	bestCount := int(^uint(0) >> 1)
	for i, stripe := range stripes {
		n := occupancy[stripe.VLANTag]
		if n >= SharedStripeSlots {
			continue
		}
		if n < bestCount {
			best = i
			bestCount = n
		}
	}
	if best < 0 {
		return Stripe{}, false, true
	}
	return stripes[best], true, false
}
