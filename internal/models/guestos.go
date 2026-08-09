package models

import (
	"regexp"
	"strings"
)

// GuestOSOption is one entry in the guest OS catalog the template wizard
// offers when building a VM from an ISO. GuestID is the vSphere guest OS
// identifier passed to CreateBlankVM; OSType is the generalization family
// ("linux" or "windows") the rest of the pipeline branches on.
//
// The catalog is deliberately broad and is NOT an allowlist: ValidGuestID
// also accepts any shape-valid custom identifier so an instructor can type a
// brand-new or uncommon vSphere guest ID (especially for a manual install)
// without waiting for a code change. The catalog just powers a friendly,
// grouped dropdown so the common cases are one click for a novice.
type GuestOSOption struct {
	// Label is the human-friendly name shown in the dropdown, e.g.
	// "Ubuntu (Server or Desktop, 64-bit)".
	Label string `json:"label"`
	// GuestID is the vSphere guest OS identifier, e.g. "ubuntu64Guest".
	GuestID string `json:"guest_id"`
	// OSType is the generalization family: "linux" or "windows". It seeds
	// the wizard's os_type so cloud-init vs sysprep generalization is chosen
	// correctly.
	OSType string `json:"os_type"`
	// Group buckets the option in the UI ("Linux", "Windows", "Other").
	Group string `json:"group"`
	// Note is optional guidance (e.g. which unattend mode fits, or a known
	// caveat). May be empty.
	Note string `json:"note,omitempty"`
}

// OS family constants for GuestOSOption.OSType. These mirror the two
// generalization paths the provisioner supports (cloud-init clean vs sysprep).
const (
	OSTypeLinux   = "linux"
	OSTypeWindows = "windows"
)

// GuestOSCatalog is the curated, ordered list of guest OS options the wizard
// presents. Every GuestID here was verified to exist in the govmomi
// VirtualMachineGuestOsIdentifier enum (vSphere 8) so vCenter accepts it on
// CreateBlankVM. Ordering is intentional: the OSes the lab's instructor guides
// cover come first within each group.
//
// This is the single source of truth shared by the API (validation +
// GET /admin/guest-os-catalog) and the UI dropdown. Add an OS here and it
// appears in the wizard with no UI change.
var GuestOSCatalog = []GuestOSOption{
	// ---- Linux (guide OSes first) ----
	{Label: "Ubuntu (Server or Desktop, 64-bit)", GuestID: "ubuntu64Guest", OSType: OSTypeLinux, Group: "Linux", Note: "Server: cloudinit_cidata. Desktop 23.04+: cloudinit_cidata (subiquity)."},
	{Label: "Linux Mint (64-bit)", GuestID: "ubuntu64Guest", OSType: OSTypeLinux, Group: "Linux", Note: "Mint is Ubuntu-based; use ubuntu64Guest. Install mode: manual."},
	{Label: "Debian 13 (Trixie, 64-bit)", GuestID: "debian13_64Guest", OSType: OSTypeLinux, Group: "Linux", Note: "Install mode: manual (debian_preseed remaster not supported yet)."},
	{Label: "Debian 12 (Bookworm, 64-bit)", GuestID: "debian12_64Guest", OSType: OSTypeLinux, Group: "Linux", Note: "Install mode: manual (debian_preseed remaster not supported yet)."},
	{Label: "Debian 11 (Bullseye, 64-bit)", GuestID: "debian11_64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "Kali Linux (64-bit)", GuestID: "debian12_64Guest", OSType: OSTypeLinux, Group: "Linux", Note: "Kali is Debian-based; use debian12_64Guest."},
	{Label: "Fedora (64-bit)", GuestID: "fedora64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "Red Hat Enterprise Linux 9 (64-bit)", GuestID: "rhel9_64Guest", OSType: OSTypeLinux, Group: "Linux", Note: "Also use for Rocky/AlmaLinux 9 if a dedicated entry isn't listed."},
	{Label: "Red Hat Enterprise Linux 8 (64-bit)", GuestID: "rhel8_64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "Red Hat Enterprise Linux 7 (64-bit)", GuestID: "rhel7_64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "Rocky Linux (64-bit)", GuestID: "rockylinux_64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "AlmaLinux (64-bit)", GuestID: "almalinux_64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "CentOS 9 Stream (64-bit)", GuestID: "centos9_64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "CentOS 8 (64-bit)", GuestID: "centos8_64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "CentOS 7 (64-bit)", GuestID: "centos7_64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "openSUSE (64-bit)", GuestID: "opensuse64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "SUSE Linux Enterprise 15 (64-bit)", GuestID: "sles15_64Guest", OSType: OSTypeLinux, Group: "Linux"},
	{Label: "Arch Linux / other modern Linux (64-bit)", GuestID: "other6xLinux64Guest", OSType: OSTypeLinux, Group: "Linux", Note: "vSphere has no Arch-specific ID; other6xLinux64Guest is the closest 3.x+ kernel match."},
	{Label: "Other Linux (64-bit)", GuestID: "otherLinux64Guest", OSType: OSTypeLinux, Group: "Linux", Note: "Generic 64-bit Linux catch-all."},

	// ---- Windows (guide OSes first) ----
	{Label: "Windows 11 (64-bit)", GuestID: "windows11_64Guest", OSType: OSTypeWindows, Group: "Windows", Note: "TPM/Secure Boot Setup checks are bypassed automatically (#121)."},
	{Label: "Windows 10 (64-bit)", GuestID: "windows9_64Guest", OSType: OSTypeWindows, Group: "Windows", Note: "Win10 uses the windows9 family ID; don't 'correct' it to windows10."},
	{Label: "Windows Server 2025 (64-bit)", GuestID: "windows2022srvNext_64Guest", OSType: OSTypeWindows, Group: "Windows", Note: "Builds on an LSI SAS controller automatically (#122)."},
	{Label: "Windows Server 2022 (64-bit)", GuestID: "windows2019srvNext_64Guest", OSType: OSTypeWindows, Group: "Windows", Note: "Builds on an LSI SAS controller automatically (#122)."},
	{Label: "Windows Server 2019 (64-bit)", GuestID: "windows2019srv_64Guest", OSType: OSTypeWindows, Group: "Windows", Note: "Builds on an LSI SAS controller automatically (#122)."},
	{Label: "Windows Server 2016 (64-bit)", GuestID: "windows9Server64Guest", OSType: OSTypeWindows, Group: "Windows", Note: "WS2016 shares the Win10 kernel, so the ID says windows9Server."},

	// ---- Other ----
	{Label: "FreeBSD 14 (64-bit)", GuestID: "freebsd14_64Guest", OSType: OSTypeLinux, Group: "Other", Note: "Install mode: manual. Generalization is a no-op; treat as manual."},
	{Label: "FreeBSD 13 (64-bit)", GuestID: "freebsd13_64Guest", OSType: OSTypeLinux, Group: "Other", Note: "Install mode: manual."},
	{Label: "Other 64-bit OS", GuestID: "otherGuest64", OSType: OSTypeLinux, Group: "Other", Note: "Last-resort catch-all for any 64-bit OS vSphere doesn't list."},
}

// guestIDShape matches the vSphere guest OS identifier convention: a run of
// alphanumerics/underscores ending in "Guest" (e.g. ubuntu64Guest,
// windows2019srvNext_64Guest). This is what lets ValidGuestID accept a
// brand-new or uncommon identifier that isn't in GuestOSCatalog yet — the
// wizard's "Other (advanced)" free-text path — so the accepted range stays
// wide and future-proof. The only well-known IDs that don't match this shape
// (otherGuest64) are already in the catalog and accepted via IsKnownGuestID.
var guestIDShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*Guest$`)

// IsKnownGuestID reports whether id is an exact GuestID in GuestOSCatalog.
func IsKnownGuestID(id string) bool {
	for _, o := range GuestOSCatalog {
		if o.GuestID == id {
			return true
		}
	}
	return false
}

// ValidGuestID reports whether id is an acceptable vSphere guest OS
// identifier for an ISO build. It is intentionally permissive: any catalog
// entry OR any identifier matching the vSphere "<name>Guest" shape passes, so
// instructors can use OSes the catalog doesn't enumerate yet. It rejects only
// clearly-wrong input (empty, spaces, or a value that doesn't look like a
// guest ID at all), which is what turns the opaque provision-time
// "guest ID required" fault into a helpful message at draft time.
func ValidGuestID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if IsKnownGuestID(id) {
		return true
	}
	return guestIDShape.MatchString(id)
}
