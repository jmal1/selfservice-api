package models

import "testing"

// guideGuestIDs are the guest OS identifiers the instructor os-recipes guide
// tells students to use. Every one MUST be selectable from the catalog, or the
// guide is unfollowable in the wizard (the exact defect that shipped: the guide
// named a Guest OS ID with no way to pick it).
var guideGuestIDs = []string{
	"ubuntu64Guest",              // Ubuntu Server/Desktop + Mint
	"debian12_64Guest",           // Debian 12 + Kali
	"debian13_64Guest",           // Debian 13
	"windows9_64Guest",           // Windows 10
	"windows11_64Guest",          // Windows 11
	"windows9Server64Guest",      // Windows Server 2016
	"windows2019srv_64Guest",     // Windows Server 2019
	"windows2019srvNext_64Guest", // Windows Server 2022
	"windows2022srvNext_64Guest", // Windows Server 2025
}

func TestGuestOSCatalog_WellFormed(t *testing.T) {
	if len(GuestOSCatalog) == 0 {
		t.Fatal("GuestOSCatalog is empty")
	}
	for i, o := range GuestOSCatalog {
		if o.Label == "" {
			t.Errorf("entry %d (%s): empty Label", i, o.GuestID)
		}
		if o.GuestID == "" {
			t.Errorf("entry %d (%q): empty GuestID", i, o.Label)
		}
		if o.OSType != OSTypeLinux && o.OSType != OSTypeWindows {
			t.Errorf("entry %d (%s): OSType = %q; want %q or %q", i, o.GuestID, o.OSType, OSTypeLinux, OSTypeWindows)
		}
		if o.Group == "" {
			t.Errorf("entry %d (%s): empty Group", i, o.GuestID)
		}
		// Every shipped GuestID must itself pass ValidGuestID, otherwise the
		// dropdown could offer a value the draft handler then rejects.
		if !ValidGuestID(o.GuestID) {
			t.Errorf("entry %d: catalog GuestID %q fails ValidGuestID", i, o.GuestID)
		}
		if !IsKnownGuestID(o.GuestID) {
			t.Errorf("entry %d: catalog GuestID %q not found by IsKnownGuestID", i, o.GuestID)
		}
	}
}

func TestGuestOSCatalog_CoversGuideOSes(t *testing.T) {
	for _, id := range guideGuestIDs {
		if !IsKnownGuestID(id) {
			t.Errorf("guide guest ID %q is missing from GuestOSCatalog — the os-recipes guide would be unfollowable in the wizard", id)
		}
	}
}

func TestValidGuestID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		// Known catalog entries.
		{"ubuntu64Guest", true},
		{"windows11_64Guest", true},
		{"windows2022srvNext_64Guest", true},
		{"otherGuest64", true}, // catch-all whose shape doesn't end in "Guest" but is catalog-known
		// Unknown but shape-valid (future / uncommon OS via the "Other" field).
		{"someFutureDistro64Guest", true},
		{"vmwarePhoton64Guest", true},
		{"debian99_64Guest", true},
		// Invalid.
		{"", false},
		{"   ", false},
		{"ubuntu", false},
		{"windows 11", false},
		{"not-a-guest", false},
		{"drop table templates", false},
	}
	for _, c := range cases {
		if got := ValidGuestID(c.id); got != c.want {
			t.Errorf("ValidGuestID(%q) = %v; want %v", c.id, got, c.want)
		}
	}
}

func TestValidGuestID_TrimsWhitespace(t *testing.T) {
	if !ValidGuestID("  ubuntu64Guest  ") {
		t.Error("ValidGuestID should accept a known ID surrounded by whitespace after trimming")
	}
}
