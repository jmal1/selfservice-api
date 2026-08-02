// Package unattend renders the "seed" media that automates an unattended OS
// install for Crucible's ISO-source templates.
//
// For source_type=iso templates, Crucible provisions a blank VM, attaches the
// installer ISO, and optionally attaches a second seed ISO that drives the
// installer non-interactively. This package generates that seed ISO for the
// three automated install families Crucible supports:
//
//   - cloudinit_cidata     Ubuntu Server (subiquity autoinstall) via a CIDATA
//     NoCloud seed ISO.
//   - windows_autounattend Windows Setup via an autounattend.xml on removable
//     media.
//   - debian_preseed       Debian/Kali via a preseeded installer. Because
//     debian-installer will not read a preseed from a
//     second CD without a kernel boot parameter, this
//     mode remasters the installer ISO instead of
//     producing a standalone seed ISO (see preseed.go).
//
// Two downstream contracts MUST hold:
//
//   - The Linux default user is "student" with passwordless sudo. Crucible
//     injects per-clone passwords via cloud-init's VMware datasource, which
//     applies the password to the default user; if the default user is not
//     "student" every clone is unusable.
//   - The Ubuntu autoinstall seed ISO must have the volume label "CIDATA" or
//     cloud-init will not discover it.
package unattend

import (
	"errors"
	"fmt"
)

// Mode values mirror the DB CHECK constraint on templates.unattend_mode.
const (
	ModeManual              = "manual"
	ModeCloudInitCIData     = "cloudinit_cidata"
	ModeDebianPreseed       = "debian_preseed"
	ModeWindowsAutounattend = "windows_autounattend"
)

// Default values applied to a Spec when the corresponding field is empty.
const (
	DefaultUsername = "student"
	DefaultLocale   = "en_US.UTF-8"
	DefaultTimeZone = "America/New_York"
)

// Sentinel errors. Callers should test these with errors.Is.
var (
	// ErrManualMode is returned by BuildSeedISO for ModeManual: a manual
	// install has no seed ISO by definition.
	ErrManualMode = errors.New("unattend: manual mode has no seed ISO")

	// ErrUnknownMode is returned by BuildSeedISO for an unrecognized mode.
	// It is deliberately distinct from ErrManualMode so a typo in the mode
	// never silently degrades to a manual (interactive) install.
	ErrUnknownMode = errors.New("unattend: unknown unattend mode")

	// ErrPreseedRequiresRemaster is returned by BuildSeedISO for
	// ModeDebianPreseed: debian-installer cannot read a preseed from a
	// second seed CD, so there is no standalone seed ISO for this mode.
	// Callers must use RemasterPreseedISO to inject the preseed into the
	// installer ISO instead.
	ErrPreseedRequiresRemaster = errors.New("unattend: debian_preseed does not produce a standalone seed ISO; use RemasterPreseedISO")
)

// Spec describes the automation to bake into a seed ISO.
type Spec struct {
	Mode      string
	Hostname  string
	Username  string // defaults to "student" when empty
	Password  string
	Locale    string // defaults "en_US.UTF-8"
	TimeZone  string // defaults "America/New_York"
	AptProxy  string // e.g. "http://10.10.30.20:3142"
	ExtraPkgs []string
}

// withDefaults returns a copy of s with empty defaultable fields populated.
func (s Spec) withDefaults() Spec {
	if s.Username == "" {
		s.Username = DefaultUsername
	}
	if s.Locale == "" {
		s.Locale = DefaultLocale
	}
	if s.TimeZone == "" {
		s.TimeZone = DefaultTimeZone
	}
	return s
}

// String implements fmt.Stringer and redacts the password so a Spec is safe
// to log. Because Spec is passed by value, this also governs how it renders
// under fmt verbs such as %v and %s.
func (s Spec) String() string {
	pw := "\"\""
	if s.Password != "" {
		pw = "<redacted>"
	}
	return fmt.Sprintf(
		"unattend.Spec{Mode:%q Hostname:%q Username:%q Password:%s Locale:%q TimeZone:%q AptProxy:%q ExtraPkgs:%v}",
		s.Mode, s.Hostname, s.Username, pw, s.Locale, s.TimeZone, s.AptProxy, s.ExtraPkgs,
	)
}

// BuildSeedISO renders the seed ISO for s.Mode. It returns the suggested
// filename and the ISO bytes.
//
//   - ModeManual returns ErrManualMode (there is no seed ISO).
//   - ModeDebianPreseed returns ErrPreseedRequiresRemaster (use
//     RemasterPreseedISO).
//   - An unknown mode returns ErrUnknownMode. This never silently falls back
//     to a manual install.
func BuildSeedISO(s Spec) (name string, data []byte, err error) {
	s = s.withDefaults()

	switch s.Mode {
	case ModeManual:
		return "", nil, ErrManualMode
	case ModeCloudInitCIData:
		return buildCIDataISO(s)
	case ModeWindowsAutounattend:
		return buildAutounattendISO(s)
	case ModeDebianPreseed:
		return "", nil, ErrPreseedRequiresRemaster
	default:
		return "", nil, fmt.Errorf("%w: %q", ErrUnknownMode, s.Mode)
	}
}
