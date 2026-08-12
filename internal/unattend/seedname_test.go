package unattend

import "testing"

func TestIsSeedISOFilename(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"tpl-x-seed-cidata.iso", true},
		{"TPL-X-SEED-CIDATA.ISO", true},
		{"foo-seed-autounattend.iso", true},
		{"linuxmint-22.3-mate-64bit.iso", false},
		{"something-seed.iso", false},
	}
	for _, tc := range cases {
		if got := IsSeedISOFilename(tc.name); got != tc.want {
			t.Errorf("IsSeedISOFilename(%q) = %v; want %v", tc.name, got, tc.want)
		}
	}
}
