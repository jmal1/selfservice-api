package models

import "testing"

func TestSharedStripesMatchTheAddressPlan(t *testing.T) {
	stripes := SharedStripes()
	if len(stripes) != SharedStripeCount {
		t.Fatalf("len = %d", len(stripes))
	}
	first := stripes[0]
	if first.VLANTag != 347 || first.CIDR != "10.110.0.0/26" || first.Gateway != "10.110.0.1/26" {
		t.Fatalf("first = %+v", first)
	}
	if first.RunnerFirst != "10.110.0.2" || first.RunnerLast != "10.110.0.15" {
		t.Fatalf("runners = %s-%s", first.RunnerFirst, first.RunnerLast)
	}
	addresses := first.RunnerAddresses()
	if len(addresses) != SharedRunnerSlots || addresses[0] != "10.110.0.2" || addresses[len(addresses)-1] != "10.110.0.15" {
		t.Fatalf("runner addresses = %v", addresses)
	}
	if first.DHCPStart != "10.110.0.16" || first.DHCPEnd != "10.110.0.47" || first.PortGroup != "Shared-VLAN347" {
		t.Fatalf("dhcp/portgroup = %+v", first)
	}
	last := stripes[len(stripes)-1]
	if last.VLANTag != 355 || last.CIDR != "10.110.2.0/26" || last.Gateway != "10.110.2.1/26" {
		t.Fatalf("last = %+v", last)
	}
	second := stripes[1]
	if second.CIDR != "10.110.0.64/26" || second.Gateway != "10.110.0.65/26" || second.DHCPStart != "10.110.0.80" {
		t.Fatalf("second = %+v", second)
	}
}

func TestPickStripeIgnoresTemplateAndFillsTheLastTag(t *testing.T) {
	active := map[int]bool{}
	occupancy := map[int]int{}
	for tag := SharedVLANTagFirst; tag <= SharedVLANTagLast; tag++ {
		active[tag] = true
		occupancy[tag] = SharedStripeSlots
	}
	occupancy[355] = 0
	got, ok, full := PickStripe(occupancy, active)
	if !ok || full || got.VLANTag != 355 {
		t.Fatalf("129th vm: ok=%v full=%v tag=%d", ok, full, got.VLANTag)
	}
	occupancy[355] = SharedStripeSlots
	_, ok, full = PickStripe(occupancy, active)
	if ok || !full {
		t.Fatalf("145th vm: ok=%v full=%v", ok, full)
	}
}

func TestPickStripeRefusesUntilEveryStripeIsActive(t *testing.T) {
	active := map[int]bool{}
	for tag := SharedVLANTagFirst; tag <= SharedVLANTagLast; tag++ {
		active[tag] = true
	}
	active[350] = false
	_, ok, full := PickStripe(map[int]int{}, active)
	if ok || full {
		t.Fatalf("inactive stripe: ok=%v full=%v", ok, full)
	}
}

func TestPickStripeBreaksTiesTowardTheLowerTag(t *testing.T) {
	active := map[int]bool{}
	for tag := SharedVLANTagFirst; tag <= SharedVLANTagLast; tag++ {
		active[tag] = true
	}
	got, ok, full := PickStripe(map[int]int{347: 1, 348: 1}, active)
	if !ok || full || got.VLANTag != 349 {
		t.Fatalf("tie = %+v ok=%v full=%v", got, ok, full)
	}
}

func TestPodCreateNetworkSkipsMutationForSharedPods(t *testing.T) {
	name, mutate := PodCreateNetwork(NetworkModeShared, 347)
	if name != "Shared-VLAN347" || mutate {
		t.Fatalf("shared = %s mutate=%v", name, mutate)
	}
	name, mutate = PodCreateNetwork(NetworkModeIsolated, 119)
	if name != "Pod-VLAN119" || !mutate {
		t.Fatalf("isolated = %s mutate=%v", name, mutate)
	}
	name, mutate = PodCreateNetwork("", 119)
	if name != "Pod-VLAN119" || !mutate {
		t.Fatalf("empty mode = %s mutate=%v", name, mutate)
	}
}
