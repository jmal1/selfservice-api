package unattend

import (
	"encoding/xml"
	"strings"
	"testing"
)

// The five HKLM\SYSTEM\Setup\LabConfig REG_DWORD values Windows 11 Setup reads
// before the disk-selection screen. Writing all five = 1 makes Setup skip the
// TPM 2.0 / Secure Boot / RAM / storage / CPU gates on Crucible's blank VM
// shells, which ship with neither a vTPM nor Secure Boot.
var win11BypassKeys = []string{
	"BypassTPMCheck",
	"BypassSecureBootCheck",
	"BypassRAMCheck",
	"BypassStorageCheck",
	"BypassCPUCheck",
}

// Structural (not byte-golden) view of the answer file, mirroring how the
// existing autounattend tests parse with encoding/xml.
type peUnattend struct {
	XMLName  xml.Name     `xml:"unattend"`
	Settings []peSettings `xml:"settings"`
}

type peSettings struct {
	Pass       string        `xml:"pass,attr"`
	Components []peComponent `xml:"component"`
}

type peComponent struct {
	Name     string             `xml:"name,attr"`
	Commands []peRunSyncCommand `xml:"RunSynchronous>RunSynchronousCommand"`
}

type peRunSyncCommand struct {
	Order      int    `xml:"Order"`
	Path       string `xml:"Path"`
	WillReboot string `xml:"WillReboot"`
}

func TestAutounattend_Win11Bypass_EmitsLabConfigWindowsPEPass(t *testing.T) {
	xmlStr, err := buildAutounattendXML(Spec{
		Hostname:                  "winlab",
		Password:                  testPlaintextPassword,
		BypassWin11HardwareChecks: true,
	})
	if err != nil {
		t.Fatalf("buildAutounattendXML: %v", err)
	}

	var doc peUnattend
	if err := xml.Unmarshal([]byte(xmlStr), &doc); err != nil {
		t.Fatalf("answer file is not well-formed XML: %v\n%s", err, xmlStr)
	}

	// The windowsPE pass must exist AND precede specialize/oobeSystem, because
	// the LabConfig keys only take effect if written before disk selection,
	// which happens during windowsPE.
	peIdx, specIdx := -1, -1
	for i, s := range doc.Settings {
		switch s.Pass {
		case "windowsPE":
			peIdx = i
		case "specialize":
			specIdx = i
		}
	}
	if peIdx < 0 {
		t.Fatalf("no <settings pass=\"windowsPE\"> block found:\n%s", xmlStr)
	}
	if specIdx < 0 {
		t.Fatalf("specialize pass unexpectedly missing")
	}
	if peIdx > specIdx {
		t.Errorf("windowsPE pass (index %d) must come before specialize (index %d) so LabConfig is set before disk selection", peIdx, specIdx)
	}

	// Collect the RunSynchronousCommands from the Deployment component in the
	// windowsPE pass and assert each of the five keys is set REG_DWORD = 1,
	// ordered 1..5.
	var cmds []peRunSyncCommand
	for _, c := range doc.Settings[peIdx].Components {
		if c.Name == "Microsoft-Windows-Deployment" {
			cmds = append(cmds, c.Commands...)
		}
	}
	if len(cmds) != len(win11BypassKeys) {
		t.Fatalf("expected %d LabConfig RunSynchronousCommands, got %d", len(win11BypassKeys), len(cmds))
	}

	for i, key := range win11BypassKeys {
		cmd := cmds[i]
		if cmd.Order != i+1 {
			t.Errorf("command for %s has Order %d, want %d", key, cmd.Order, i+1)
		}
		if cmd.WillReboot != "Never" {
			t.Errorf("command for %s has WillReboot %q, want Never", key, cmd.WillReboot)
		}
		if !strings.Contains(cmd.Path, `reg add "HKLM\SYSTEM\Setup\LabConfig"`) {
			t.Errorf("command %d does not target HKLM\\SYSTEM\\Setup\\LabConfig: %q", i+1, cmd.Path)
		}
		if !strings.Contains(cmd.Path, "/v "+key+" ") {
			t.Errorf("command %d does not set key %s: %q", i+1, key, cmd.Path)
		}
		if !strings.Contains(cmd.Path, "/t REG_DWORD /d 1 /f") {
			t.Errorf("command for %s is not REG_DWORD=1: %q", key, cmd.Path)
		}
	}
}

func TestAutounattend_NoBypass_OmitsWindowsPEAndKeepsLayout(t *testing.T) {
	xmlStr, err := buildAutounattendXML(Spec{
		Hostname:                  "winlab",
		Password:                  testPlaintextPassword,
		BypassWin11HardwareChecks: false,
	})
	if err != nil {
		t.Fatalf("buildAutounattendXML: %v", err)
	}

	// No windowsPE pass and no LabConfig anywhere: Win10/Server output must be
	// exactly what it was before this feature existed.
	if strings.Contains(xmlStr, "windowsPE") {
		t.Errorf("answer file must not contain a windowsPE pass when the flag is off:\n%s", xmlStr)
	}
	if strings.Contains(xmlStr, "LabConfig") {
		t.Errorf("answer file must not contain LabConfig when the flag is off:\n%s", xmlStr)
	}

	// The exact header whitespace must be untouched, proving the {{if}} guard
	// added no stray lines around the specialize pass.
	const wantHead = "<unattend xmlns=\"urn:schemas-microsoft-com:unattend\">\n\n  <settings pass=\"specialize\">"
	if !strings.Contains(xmlStr, wantHead) {
		t.Errorf("header layout changed when flag is off; expected to find:\n%q\nin:\n%s", wantHead, xmlStr)
	}

	// Still exactly the two original passes.
	var doc peUnattend
	if err := xml.Unmarshal([]byte(xmlStr), &doc); err != nil {
		t.Fatalf("answer file is not well-formed XML: %v", err)
	}
	if len(doc.Settings) != 2 {
		t.Fatalf("expected 2 settings passes (specialize, oobeSystem), got %d", len(doc.Settings))
	}
	if doc.Settings[0].Pass != "specialize" || doc.Settings[1].Pass != "oobeSystem" {
		t.Errorf("passes = [%q %q], want [specialize oobeSystem]", doc.Settings[0].Pass, doc.Settings[1].Pass)
	}
}
