package vcenter

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveHostIdentitiesStrict(t *testing.T) {
	inventory := []HostIdentity{
		{
			Name:          "esxi1.lab.jmal.io",
			InventoryPath: "/DC0/host/Intel/esxi1.lab.jmal.io",
			MoRef:         "host-101",
			ComputeMoRef:  "domain-c7",
		},
		{
			Name:          "esxi2.lab.jmal.io",
			InventoryPath: "/DC0/host/Intel/esxi2.lab.jmal.io",
			MoRef:         "host-102",
			ComputeMoRef:  "domain-c7",
		},
	}

	got, err := resolveHostIdentities([]string{"ESXI1.lab.jmal.io"}, inventory)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MoRef != "host-101" {
		t.Fatalf("resolved hosts = %+v, want ESXi1 only", got)
	}

	tests := []struct {
		name       string
		configured []string
		inventory  []HostIdentity
		want       string
	}{
		{
			name:       "empty",
			configured: nil,
			inventory:  inventory,
			want:       "at least one",
		},
		{
			name:       "unresolvable",
			configured: []string{"missing.lab.jmal.io"},
			inventory:  inventory,
			want:       "does not resolve",
		},
		{
			name:       "ambiguous name",
			configured: []string{"esxi1.lab.jmal.io"},
			inventory: append(append([]HostIdentity(nil), inventory...), HostIdentity{
				Name:          "esxi1.lab.jmal.io",
				InventoryPath: "/DC1/host/Intel/esxi1.lab.jmal.io",
				MoRef:         "host-201",
				ComputeMoRef:  "domain-c8",
			}),
			want: "ambiguous",
		},
		{
			name:       "duplicate immutable identity",
			configured: []string{"esxi1.lab.jmal.io", "/DC0/host/Intel/esxi1.lab.jmal.io"},
			inventory:  inventory,
			want:       "same immutable host",
		},
		{
			name:       "incomplete identity",
			configured: []string{"esxi1.lab.jmal.io"},
			inventory: []HostIdentity{{
				Name:          "esxi1.lab.jmal.io",
				InventoryPath: "/DC0/host/Intel/esxi1.lab.jmal.io",
				MoRef:         "host-101",
			}},
			want: "complete inventory identity",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveHostIdentities(tc.configured, tc.inventory)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want text %q", err, tc.want)
			}
		})
	}
}

func TestAllowedHostLookupFailsClosed(t *testing.T) {
	client := &Client{allowedHosts: []HostIdentity{{
		Name:         "esxi1.lab.jmal.io",
		MoRef:        "host-101",
		ComputeMoRef: "domain-c7",
	}}}

	if _, err := client.allowedHostByMoRef("host-101"); err != nil {
		t.Fatalf("allowlisted host rejected: %v", err)
	}
	if _, err := client.allowedHostByMoRef("host-102"); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("disallowed host error = %v, want ErrHostNotAllowed", err)
	}
}
