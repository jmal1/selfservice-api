package vcenter

import "testing"

func TestIsVMMoref(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"vm-8942", true},
		{"vm-1", true},
		{"vm-", false},
		{"vm-abc", false},
		{"vm-12a", false},
		{"student-windows-11", false},
		{"", false},
		{"VM-8942", false},
		{"vm-8942 ", false},
		{"host-12", false},
	}
	for _, c := range cases {
		if got := isVMMoref(c.in); got != c.want {
			t.Errorf("isVMMoref(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
