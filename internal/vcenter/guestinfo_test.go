package vcenter

import (
	"testing"

	"github.com/vmware/govmomi/vim25/types"
)

// TestPickGuestIPv4 locks in that the wizard-facing guest IP is always a
// routable IPv4, never an IPv6 link-local — the June/July build VMs reported
// fe80::… as guest.ipAddress, which the UI turned into a dead
// `mstsc /v:fe80::…` RDP link.
func TestPickGuestIPv4(t *testing.T) {
	v4 := func(s string) types.NetIpConfigInfoIpAddress {
		return types.NetIpConfigInfoIpAddress{IpAddress: s}
	}
	tests := []struct {
		name  string
		guest *types.GuestInfo
		want  string
	}{
		{
			name: "prefers routable v4 over link-local v6 primary",
			guest: &types.GuestInfo{
				IpAddress: "fe80::bea3:6eb1:1cc5:3b4",
				Net: []types.GuestNicInfo{{
					IpConfig: &types.NetIpConfigInfo{IpAddress: []types.NetIpConfigInfoIpAddress{
						v4("fe80::bea3:6eb1:1cc5:3b4"),
						v4("10.10.30.137"),
					}},
				}},
			},
			want: "10.10.30.137",
		},
		{
			name: "skips APIPA link-local v4",
			guest: &types.GuestInfo{
				Net: []types.GuestNicInfo{{
					IpConfig: &types.NetIpConfigInfo{IpAddress: []types.NetIpConfigInfoIpAddress{
						v4("169.254.229.151"),
					}},
				}},
			},
			want: "",
		},
		{
			name: "falls back to guest.ipAddress when it is a usable v4",
			guest: &types.GuestInfo{
				IpAddress: "10.10.30.150",
				Net:       nil,
			},
			want: "10.10.30.150",
		},
		{
			name: "uses legacy per-NIC IpAddress list",
			guest: &types.GuestInfo{
				Net: []types.GuestNicInfo{{
					IpAddress: []string{"fe80::1", "10.100.5.20"},
				}},
			},
			want: "10.100.5.20",
		},
		{
			name:  "nothing usable yet",
			guest: &types.GuestInfo{IpAddress: "fe80::1"},
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickGuestIPv4(tc.guest); got != tc.want {
				t.Errorf("pickGuestIPv4() = %q, want %q", got, tc.want)
			}
		})
	}
}
