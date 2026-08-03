package unattend

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// autoinstallBlock decodes the autoinstall: mapping out of a generated
// cloudinit_cidata user-data document.
func autoinstallBlock(t *testing.T, ud []byte) map[string]interface{} {
	t.Helper()
	var doc struct {
		Autoinstall map[string]interface{} `yaml:"autoinstall"`
	}
	if err := yaml.Unmarshal(ud, &doc); err != nil {
		t.Fatalf("user-data does not parse as YAML: %v", err)
	}
	if doc.Autoinstall == nil {
		t.Fatal("user-data has no autoinstall block")
	}
	return doc.Autoinstall
}

func stringSlice(t *testing.T, v interface{}, key string) []string {
	t.Helper()
	raw, ok := v.([]interface{})
	if !ok {
		t.Fatalf("autoinstall.%s is %T, want a list", key, v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("autoinstall.%s contains a %T, want strings only", key, item)
		}
		out = append(out, s)
	}
	return out
}

// TestBuildSeedISO_CIData_ShutdownIsPoweroff guards the ISO path's completion
// signal at its source.
//
// The provisioner waits for the VM to power itself off to decide the install
// finished, because VMware Tools cannot be used for this: the Ubuntu
// live-server ISO runs open-vm-tools in the ephemeral installer environment and
// reports Tools running roughly 40 seconds after power-on, with nothing yet
// written to disk.
//
// If this key goes missing, subiquity's default is "shutdown: reboot". The VM
// would reboot into the freshly installed system instead of powering off, the
// provisioner would never observe poweredOff, and every unattended build would
// sit until the 60-minute deadline and then report a timeout on an install that
// actually succeeded.
func TestBuildSeedISO_CIData_ShutdownIsPoweroff(t *testing.T) {
	_, ud := cidataUserData(t, Spec{
		Mode:     ModeCloudInitCIData,
		Hostname: "ubuntu-lab",
		Password: testPlaintextPassword,
	})

	ai := autoinstallBlock(t, ud)
	got, ok := ai["shutdown"]
	if !ok {
		t.Fatalf("autoinstall has no shutdown key: subiquity defaults to reboot, so the provisioner " +
			"would never see the VM power off and every unattended build would time out at 60m")
	}
	if got != "poweroff" {
		t.Errorf("autoinstall.shutdown = %v, want poweroff (it is the provisioner's install-complete signal)", got)
	}
}

// TestBuildSeedISO_CIData_AutoConfirmsInstall is the regression guard for the
// defect that made hands-off installs impossible.
//
// Supplying an autoinstall config over a NoCloud seed ISO is NOT sufficient.
// subiquity refuses to touch the disk until it is confirmed, and the only
// documented bypass is the bare token "autoinstall" on the guest kernel command
// line (subiquity/server/controllers/install.py: `if "autoinstall" in
// self.app.kernel_cmdline: await self.model.confirm()`). A seed ISO cannot
// deliver a kernel argument, and we cannot remaster the installer ISO to add
// one (no El Torito boot-catalog writer — see ErrRemasterUnsupported).
//
// Observed consequence before this existed: the VM booted, sat at
// "Continue with autoinstall? (yes|no)" indefinitely, and reported VMware Tools
// running the whole time because the *installer* environment runs them.
//
// So we answer the prompt from inside the installer via subiquity's own API.
// This asserts the mechanism is present and intact, not merely that some
// early-command exists.
func TestBuildSeedISO_CIData_AutoConfirmsInstall(t *testing.T) {
	_, ud := cidataUserData(t, Spec{
		Mode:     ModeCloudInitCIData,
		Hostname: "ubuntu-lab",
		Password: testPlaintextPassword,
	})

	ai := autoinstallBlock(t, ud)
	raw, ok := ai["early-commands"]
	if !ok {
		t.Fatal("autoinstall has no early-commands: without one, subiquity blocks at " +
			"\"Continue with autoinstall? (yes|no)\" forever and no unattended install can ever complete")
	}
	cmds := stringSlice(t, raw, "early-commands")
	if len(cmds) == 0 {
		t.Fatal("autoinstall.early-commands is empty; the confirmation prompt would never be answered")
	}

	joined := strings.Join(cmds, "\n")

	// The three parts that make it work at all.
	for _, want := range []struct{ frag, why string }{
		{"/meta/confirm", "the subiquity endpoint that answers the confirmation prompt"},
		{"/run/subiquity/socket", "the unix socket the subiquity server listens on"},
		{"NEEDS_CONFIRMATION", "the state we wait for, so we never confirm before the installer is ready"},
		{"python3", "curl is not guaranteed in the installer environment; python3 is, because subiquity is a python app"},
	} {
		if !strings.Contains(joined, want.frag) {
			t.Errorf("early-commands is missing %q (%s):\n%s", want.frag, want.why, joined)
		}
	}

	// early-commands abort the whole install on a non-zero exit, so this must
	// never be able to fail the run: a stalled confirmation should degrade to
	// the old timeout behaviour, not turn into an immediate abort.
	if !strings.HasSuffix(strings.TrimRight(joined, " \n"), "&") {
		t.Errorf("the auto-confirm command must be backgrounded (trailing &): early-commands run " +
			"synchronously and abort the install on a non-zero exit, and this command deliberately " +
			"polls for minutes")
	}
	if !strings.Contains(joined, "2>&1") {
		t.Errorf("the auto-confirm command must swallow its own errors; an early-command that exits " +
			"non-zero aborts the install")
	}
}

// TestBuildSeedISO_CIData_AutoConfirmDoesNotLeakPassword keeps the new
// early-command honest about the same secret contract the rest of the seed
// obeys: nothing added here may echo the plaintext password.
func TestBuildSeedISO_CIData_AutoConfirmDoesNotLeakPassword(t *testing.T) {
	if strings.Contains(autoConfirmScript, testPlaintextPassword) {
		t.Fatal("autoConfirmScript must never interpolate the password")
	}
	_, ud := cidataUserData(t, Spec{
		Mode:     ModeCloudInitCIData,
		Hostname: "ubuntu-lab",
		Password: testPlaintextPassword,
	})
	if strings.Contains(string(ud), testPlaintextPassword) {
		t.Fatal("generated user-data contains the plaintext password")
	}
}
