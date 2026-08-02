package unattend

// preseed.go handles the Debian/Kali (debian-installer) family.
//
// debian-installer will NOT read a preseed from a second CD without a kernel
// boot parameter, so unlike the other modes this one cannot ship a standalone
// seed ISO. The install must be automated by *remastering* the installer ISO:
// injecting /preseed.cfg and patching the bootloader configs to auto-load it.
//
// IMPORTANT — honest limitation: the pure-Go github.com/kdomanski/iso9660
// writer produces a plain ISO9660 image. It cannot reproduce an El Torito
// boot catalog / boot image, so any ISO it writes is NOT bootable. A Debian
// installer ISO is a hybrid BIOS+UEFI El Torito image; a remaster that drops
// the boot record would boot to nothing. Rather than emit a broken, silently
// non-bootable ISO, RemasterPreseedISO returns ErrRemasterUnsupported and the
// caller must fall back to a manual install for this template.
//
// The two pieces of real, reusable logic — generating the preseed.cfg and
// patching the bootloader config to append the preseed boot parameters — are
// implemented as independently-testable pure functions (BuildPreseedConfig and
// PatchBootloaderConfig) so they are ready to use the moment a bootable-ISO
// writer becomes available.

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kdomanski/iso9660"
)

// preseedBootParams are appended to the installer's default kernel command
// line so debian-installer runs fully unattended from /cdrom/preseed.cfg.
const preseedBootParams = "auto=true priority=critical file=/cdrom/preseed.cfg"

// ErrRemasterUnsupported is returned by RemasterPreseedISO when the installer
// ISO cannot be faithfully remastered into a bootable image with the available
// pure-Go tooling. The caller must fall back to manual mode for the template.
var ErrRemasterUnsupported = errors.New("unattend: bootable preseed remaster is unsupported with the pure-Go iso9660 writer (no El Torito boot record); fall back to manual mode")

// BuildPreseedConfig renders a debian-installer preseed.cfg for s. The
// student account password is stored as a SHA-512 crypt hash, never plaintext.
func BuildPreseedConfig(s Spec) (string, error) {
	s = s.withDefaults()

	hash, err := generateSHA512Crypt(s.Password)
	if err != nil {
		return "", fmt.Errorf("unattend: hash password: %w", err)
	}

	hostname := s.Hostname
	if hostname == "" {
		hostname = "kali"
	}

	pkgs := append([]string{"open-vm-tools", "cloud-init"}, s.ExtraPkgs...)

	var b strings.Builder
	w := func(line string) { b.WriteString(line); b.WriteByte('\n') }

	w("# Crucible-generated debian-installer preseed")
	w("")
	w("# Locale, keyboard and timezone")
	w("d-i debian-installer/locale string " + s.Locale)
	w("d-i keyboard-configuration/xkb-keymap select us")
	w("d-i time/zone string " + s.TimeZone)
	w("d-i clock-setup/utc boolean true")
	w("d-i clock-setup/ntp boolean true")
	w("")
	w("# Networking / hostname")
	w("d-i netcfg/choose_interface select auto")
	w("d-i netcfg/get_hostname string " + hostname)
	w("d-i netcfg/get_domain string lab.jmal.io")
	w("")

	if s.AptProxy != "" {
		w("# APT proxy")
		w("d-i mirror/http/proxy string " + s.AptProxy)
		w("")
	}

	w("# Account setup: no root login, student user with passwordless sudo")
	w("d-i passwd/root-login boolean false")
	w("d-i passwd/make-user boolean true")
	w("d-i passwd/user-fullname string Crucible Student")
	w("d-i passwd/username string " + s.Username)
	w("d-i passwd/user-password-crypted password " + hash)
	w("d-i user-setup/allow-password-weak boolean true")
	w("d-i user-setup/encrypt-home boolean false")
	w("")
	w("# Partitioning: guided, use entire disk (single LVM)")
	w("d-i partman-auto/method string lvm")
	w("d-i partman-lvm/device_remove_lvm boolean true")
	w("d-i partman-auto/choose_recipe select atomic")
	w("d-i partman-partitioning/confirm_write_new_label boolean true")
	w("d-i partman/choose_partition select finish")
	w("d-i partman/confirm boolean true")
	w("d-i partman/confirm_nooverwrite boolean true")
	w("")
	w("# Packages")
	w("d-i pkgsel/include string " + strings.Join(pkgs, " "))
	w("d-i pkgsel/upgrade select none")
	w("popularity-contest popularity-contest/participate boolean false")
	w("")
	w("# Grant the student user passwordless sudo inside the installed system")
	w("d-i preseed/late_command string " +
		"in-target sh -c 'echo \"" + s.Username + " ALL=(ALL) NOPASSWD:ALL\" > /etc/sudoers.d/90-crucible-student; " +
		"chmod 440 /etc/sudoers.d/90-crucible-student'")
	w("")
	w("# Finish without prompting and reboot")
	w("d-i finish-install/reboot_in_progress note")

	return b.String(), nil
}

// PatchBootloaderConfig appends the preseed boot parameters to the default
// kernel command line in a bootloader configuration (isolinux txt.cfg or GRUB
// grub.cfg). Lines already carrying the parameters are left unchanged. When a
// "---" separator is present (isolinux hands parameters after it to the booted
// system) the parameters are inserted before it.
func PatchBootloaderConfig(cfg string) string {
	lines := strings.Split(cfg, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)

		isKernelLine := strings.HasPrefix(lower, "append") ||
			strings.HasPrefix(lower, "linux ") ||
			strings.HasPrefix(lower, "linuxefi ") ||
			strings.HasPrefix(lower, "linux\t") ||
			strings.HasPrefix(lower, "linuxefi\t")
		if !isKernelLine {
			continue
		}
		if strings.Contains(line, preseedBootParams) {
			continue
		}

		if idx := strings.Index(line, "---"); idx >= 0 {
			before := strings.TrimRight(line[:idx], " \t")
			lines[i] = before + " " + preseedBootParams + " " + line[idx:]
		} else {
			lines[i] = strings.TrimRight(line, " \t") + " " + preseedBootParams
		}
	}
	return strings.Join(lines, "\n")
}

// RemasterPreseedISO reads a Debian-family installer ISO, would inject
// /preseed.cfg and patch the bootloader configs to auto-load it, and return
// the new ISO bytes.
//
// It currently returns ErrRemasterUnsupported: see the package/file comment.
// The source is still opened so genuinely-unreadable input yields a distinct
// error from the "readable but cannot be made bootable" case, giving callers a
// clear signal to fall back to manual mode.
func RemasterPreseedISO(src io.ReaderAt, srcSize int64, s Spec) (data []byte, err error) {
	if src == nil || srcSize <= 0 {
		return nil, fmt.Errorf("unattend: invalid installer ISO source")
	}

	img, err := iso9660.OpenImage(io.NewSectionReader(src, 0, srcSize))
	if err != nil {
		return nil, fmt.Errorf("unattend: read installer ISO: %w", err)
	}
	if _, err := img.RootDir(); err != nil {
		return nil, fmt.Errorf("unattend: read installer ISO root: %w", err)
	}

	// Generating the preseed and patched bootloader config succeeds; only the
	// final bootable-ISO assembly is unsupported. We compute the preseed here
	// so any generation error surfaces before the unsupported signal.
	if _, err := BuildPreseedConfig(s); err != nil {
		return nil, err
	}

	return nil, ErrRemasterUnsupported
}
