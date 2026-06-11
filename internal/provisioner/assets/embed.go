// Package assets bundles static resources that the worker uploads
// into guest VMs as part of provisioning. Keeping these alongside the
// provisioner package (and not under templates/ at the repo root)
// lets //go:embed pick them up; embed paths cannot traverse out of a
// package's directory.
//
// windows-unattend.xml is the single source of truth for the answer
// file used during both the manual L1 sysprep (see SETUP.md) and the
// Crucible wizard's L2 generalize step. The wizard re-uploads it to
// C:\Windows\Panther\unattend.xml on every generalize so that the
// scrubbed copy left behind by the previous sysprep is replaced with
// the canonical (base64-encoded-password) version before the next
// sysprep runs. Without that, the second-and-later sysprep cycles
// would emit `*SENSITIVE*DATA*DELETED*` for the Student account
// password and Windows OOBE would fall back to interactive account
// creation on every clone.
package assets

import _ "embed"

// WindowsUnattendXML is the canonical Windows unattend file uploaded
// to C:\Windows\Panther\unattend.xml before sysprep on the L2 staging
// VM. Update the .xml file (not this comment block) and re-encode the
// Student bootstrap password as
//
//	base64(UTF-16LE(<password> + "Password"))
//
// before changing the <Value> elements. PlainText=true passwords are
// scrubbed by sysprep; PlainText=false (base64) ones are preserved.
//
//go:embed windows-unattend.xml
var WindowsUnattendXML []byte
