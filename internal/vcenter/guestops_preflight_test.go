package vcenter

import "testing"

// TestParseSysprepPreflight locks the parser that turns the guest-side
// "PREFLIGHT:REARM=<n>;PENDINGREBOOT=<bool>" sentinel into a decision. The
// rearm==0 case is the one that must never regress — it's the guaranteed
// "sysprep will brick the image" signal.
func TestParseSysprepPreflight(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantRearm   int
		wantPending bool
	}{
		{"healthy", "PREFLIGHT:REARM=1001;PENDINGREBOOT=False", 1001, false},
		{"exhausted", "PREFLIGHT:REARM=0;PENDINGREBOOT=False", 0, false},
		{"pending reboot true", "PREFLIGHT:REARM=3;PENDINGREBOOT=True", 3, true},
		{"undetermined", "PREFLIGHT:REARM=-1;PENDINGREBOOT=False", -1, false},
		{"noise around sentinel", "some banner\r\nPREFLIGHT:REARM=5;PENDINGREBOOT=true\r\n", 5, true},
		{"no sentinel", "totally unrelated output", -1, false},
		{"garbage rearm falls back to -1", "PREFLIGHT:REARM=abc;PENDINGREBOOT=False", -1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rearm, pending := parseSysprepPreflight(c.in)
			if rearm != c.wantRearm {
				t.Errorf("rearm = %d; want %d", rearm, c.wantRearm)
			}
			if pending != c.wantPending {
				t.Errorf("pending = %v; want %v", pending, c.wantPending)
			}
		})
	}
}
