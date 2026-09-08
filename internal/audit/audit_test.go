package audit

import "testing"

// TestNormalizeRemoteAddr exists because normalizeRemoteAddr was written,
// left untested, and then bypassed: ~30 handlers call IP(r.RemoteAddr)
// directly instead of FromRequest, so the port survived all the way to an
// INET column and every insert failed with SQLSTATE 22P02 — silently, since
// Log only logs its write error. Log now normalizes, and this pins the
// behaviour that makes that safe.
func TestNormalizeRemoteAddr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The case that broke production: net/http always appends a port.
		{"ipv4 with port", "192.0.2.10:54321", "192.0.2.10"},
		{"ipv6 with port", "[2001:db8::1]:8443", "2001:db8::1"},
		// Idempotency. Log normalizes unconditionally, so anything already
		// bare — including a value that went through FromRequest — must pass
		// through unchanged rather than being mangled a second time.
		{"bare ipv4 unchanged", "192.0.2.10", "192.0.2.10"},
		{"bare ipv6 unchanged", "2001:db8::1", "2001:db8::1"},
		{"bracketed ipv6 unwrapped", "[2001:db8::1]", "2001:db8::1"},
		// Empty stays empty so Log leaves ip_address NULL instead of
		// inserting a string INET would reject.
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRemoteAddr(tc.in); got != tc.want {
				t.Errorf("normalizeRemoteAddr(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Whatever comes out must survive a second pass, because Log
			// may normalize a value FromRequest already normalized.
			if got := normalizeRemoteAddr(normalizeRemoteAddr(tc.in)); got != tc.want {
				t.Errorf("normalizeRemoteAddr is not idempotent for %q: got %q, want %q",
					tc.in, got, tc.want)
			}
		})
	}
}
