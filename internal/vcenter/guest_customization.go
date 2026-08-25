package vcenter

import (
	"encoding/base64"
	"fmt"

	"github.com/vmware/govmomi/vim25/types"
)

// guestinfoCustomization builds the guestinfo.userdata / guestinfo.metadata
// ExtraConfig options that carry a first-boot password (and hostname) into a
// freshly cloned VM's guest OS. Linux consumes it via cloud-init's VMware
// datasource; Windows via cloudbase-init.
//
// Both clone paths use this single builder so they cannot drift:
//
//   - cloneVMInner            — the per-pod student clone
//   - cloneTemplateSourceVMInner — the template-wizard staging clone
//
// A divergence between the two would silently lock one path's users out while
// the other kept working — exactly the class of failure that produced the
// "instructor can't log into the staging VM with the supplied credentials"
// bug: the wizard clone injected nothing, so the source image's cloud-init
// `student` account (which has no baked-in password — it is set per-clone)
// booted with no usable password.
//
// The template staging path keeps its historically unique VM name as the
// first-boot identity. Production clones call
// guestinfoCustomizationForInstance with their durable operation identity.
func guestinfoCustomization(osType, password, hostname string) []types.BaseOptionValue {
	extra, _ := guestinfoCustomizationForInstance(osType, password, hostname, hostname)
	return extra
}

// guestinfoCustomizationForInstance builds a password-bearing payload whose
// instanceID is unique per clone operation. Both cloud-init and cloudbase-init
// use it to decide whether first-boot userdata has already run. It returns nil
// when password is empty and fails closed if the OS or identity is unsupported.
func guestinfoCustomizationForInstance(osType, password, hostname, instanceID string) ([]types.BaseOptionValue, error) {
	if password == "" {
		return nil, nil
	}
	if hostname == "" {
		return nil, fmt.Errorf("guest customization hostname is required")
	}
	if instanceID == "" {
		return nil, fmt.Errorf("guest customization instance ID is required")
	}

	var userdata, metadata string
	switch osType {
	case "linux":
		// cloud-init format. The bare top-level `password:` applies to the
		// image's default user, which for Crucible student images is
		// `student`.
		userdata = fmt.Sprintf(`#cloud-config
password: %s
chpasswd:
  expire: false
ssh_pwauth: true
hostname: %s
`, password, hostname)
		metadata = fmt.Sprintf(`{"instance-id": "%s", "local-hostname": "%s"}`, instanceID, hostname)
	case "windows":
		// cloudbase-init: UserDataPlugin runs #ps1 script to set password.
		// SetHostNamePlugin reads local-hostname from metadata.
		// Plugin order in cloudbase-init.conf must have UserData before SetHostName
		// (SetHostName triggers a reboot).
		userdata = fmt.Sprintf(`#ps1_sysnative
$password = ConvertTo-SecureString '%s' -AsPlainText -Force
Get-LocalUser -Name 'Student' | Set-LocalUser -Password $password
`, password)
		metadata = fmt.Sprintf(`{"instance-id": "%s", "local-hostname": "%s", "admin_pass": "%s"}`,
			instanceID, hostname, password)
	default:
		return nil, fmt.Errorf("guest customization does not support OS %q", osType)
	}

	var out []types.BaseOptionValue
	if userdata != "" {
		out = append(out,
			&types.OptionValue{Key: "guestinfo.userdata", Value: base64.StdEncoding.EncodeToString([]byte(userdata))},
			&types.OptionValue{Key: "guestinfo.userdata.encoding", Value: "base64"},
		)
	}
	if metadata != "" {
		out = append(out,
			&types.OptionValue{Key: "guestinfo.metadata", Value: base64.StdEncoding.EncodeToString([]byte(metadata))},
			&types.OptionValue{Key: "guestinfo.metadata.encoding", Value: "base64"},
		)
	}
	return out, nil
}
