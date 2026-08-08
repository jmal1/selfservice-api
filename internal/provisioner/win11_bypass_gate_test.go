package provisioner

import (
	"encoding/json"
	"testing"
)

// The Win11 LabConfig bypass is gated strictly on the vSphere guest ID so that
// only windows11_64Guest gets the extra windowsPE pass. Every other guest ID
// (Windows 10 = windows9_64Guest, all Server IDs, Linux) must keep the flag
// off and therefore keep producing byte-identical seed media.
func TestUnattendSpecFromPayload_Win11HardwareBypassGate(t *testing.T) {
	cases := []struct {
		guestID string
		want    bool
	}{
		{"windows11_64Guest", true},
		{"windows9_64Guest", false}, // Windows 10
		{"windows2019srv_64Guest", false},
		{"windows2022srvNext_64Guest", false},
		{"ubuntu64Guest", false},
		{"", false},
	}

	for _, tc := range cases {
		t.Run(tc.guestID, func(t *testing.T) {
			spec, err := unattendSpecFromPayload(TemplateProvisionPayload{
				GuestID:      tc.guestID,
				UnattendMode: "windows_autounattend",
			})
			if err != nil {
				t.Fatalf("unattendSpecFromPayload: %v", err)
			}
			if spec.BypassWin11HardwareChecks != tc.want {
				t.Errorf("guest %q: BypassWin11HardwareChecks = %v, want %v", tc.guestID, spec.BypassWin11HardwareChecks, tc.want)
			}
		})
	}
}

// The bypass flag is derived from the guest ID, not authored in unattend_config,
// so it must never be settable via the JSON config blob.
func TestUnattendSpecFromPayload_BypassNotSettableFromJSON(t *testing.T) {
	cfg := json.RawMessage(`{"hostname":"win","BypassWin11HardwareChecks":true,"bypass_win11_hardware_checks":true}`)
	spec, err := unattendSpecFromPayload(TemplateProvisionPayload{
		GuestID:        "ubuntu64Guest",
		UnattendMode:   "windows_autounattend",
		UnattendConfig: cfg,
	})
	if err != nil {
		t.Fatalf("unattendSpecFromPayload: %v", err)
	}
	if spec.BypassWin11HardwareChecks {
		t.Errorf("BypassWin11HardwareChecks must not be settable via unattend_config JSON")
	}
}
