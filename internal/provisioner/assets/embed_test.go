package assets

import (
	"strings"
	"testing"
)

// TestWindowsUnattendXML_Embedded guards against the embed directive
// silently breaking (file renamed, build-tag mismatch, etc.). It also
// asserts the password format is the encoded-not-scrubbed form so a
// well-intentioned edit reverting to <PlainText>true</PlainText>
// triggers a CI failure long before the bad bytes hit production and
// every L2 generalize starts producing broken templates.
func TestWindowsUnattendXML_Embedded(t *testing.T) {
	if len(WindowsUnattendXML) == 0 {
		t.Fatal("WindowsUnattendXML is empty — //go:embed directive likely broken")
	}
	if len(WindowsUnattendXML) > 64*1024 {
		t.Fatalf("WindowsUnattendXML is %d bytes; sanity cap is 64 KiB. "+
			"Did someone embed the wrong file?", len(WindowsUnattendXML))
	}
	body := string(WindowsUnattendXML)
	if !strings.Contains(body, "<unattend") {
		t.Error("unattend.xml is missing the <unattend> root element")
	}
	if strings.Contains(body, "<PlainText>true</PlainText>") {
		t.Error(`unattend.xml contains <PlainText>true</PlainText>; ` +
			`sysprep will scrub these passwords. Use <PlainText>false</PlainText> ` +
			`with base64(UTF-16LE(password + "Password")) instead. ` +
			`See assets/embed.go docstring.`)
	}
	if strings.Contains(body, "*SENSITIVE*DATA*DELETED*") {
		t.Error("unattend.xml contains the sysprep scrub marker — someone copied a " +
			"post-sysprep file into the repo. Re-create from a clean source.")
	}
	if !strings.Contains(body, "sc config cloudbase-init start= delayed-auto") {
		t.Error("unattend.xml is missing the Server-SKU cloudbase-init delayed-auto enablement. " +
			"Windows Server 2022/2025 skip FirstLogonCommands during OOBE, so the service " +
			"must be re-enabled in the specialize pass or the generated Student password never applies.")
	}
}
