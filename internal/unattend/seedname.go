package unattend

import "strings"

// Seed ISO base filenames returned by the generators. The provisioner uploads
// these into the ISO datastore folder prefixed with "<VMName>-" (see
// internal/provisioner/template_jobs.go), so the on-datastore names end with
// "-seed-cidata.iso" / "-seed-autounattend.iso".
//
// These constants are the single source of truth shared by the generators
// (cloudinit.go, windows.go) and the wizard ISO picker filter
// (internal/api/handlers/images.go) so the two cannot drift.
const (
	SeedISONameCIData       = "seed-cidata.iso"
	SeedISONameAutounattend = "seed-autounattend.iso"
)

// IsSeedISOFilename reports whether name is a provisioner-generated seed ISO
// (cloud-init CIDATA or Windows autounattend). These are non-bootable media
// used to drive unattended installs and must never be offered as an installer
// source in the template wizard.
//
// The match is case-insensitive and requires the exact suffix WITH the leading
// hyphen ("-seed-cidata.iso" / "-seed-autounattend.iso") so a legitimately
// named installer (e.g. "something-seed.iso") is never dropped.
func IsSeedISOFilename(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, "-"+SeedISONameCIData) ||
		strings.HasSuffix(lower, "-"+SeedISONameAutounattend)
}
