package vcenter

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25/types"
	"gopkg.in/yaml.v3"
)

// decodeGuestinfo pulls the decoded userdata / metadata payloads back out of
// the ExtraConfig options the builder produced, so assertions read the actual
// guest-facing content rather than opaque base64.
func decodeGuestinfo(t *testing.T, opts []types.BaseOptionValue) (userdata, metadata string) {
	t.Helper()
	for _, o := range opts {
		ov, ok := o.(*types.OptionValue)
		if !ok {
			t.Fatalf("ExtraConfig option is %T, want *types.OptionValue", o)
		}
		val, _ := ov.Value.(string)
		switch ov.Key {
		case "guestinfo.userdata":
			b, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				t.Fatalf("userdata not valid base64: %v", err)
			}
			userdata = string(b)
		case "guestinfo.metadata":
			b, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				t.Fatalf("metadata not valid base64: %v", err)
			}
			metadata = string(b)
		case "guestinfo.userdata.encoding", "guestinfo.metadata.encoding":
			if val != "base64" {
				t.Fatalf("%s = %q, want base64", ov.Key, val)
			}
		default:
			t.Fatalf("unexpected ExtraConfig key %q", ov.Key)
		}
	}
	return userdata, metadata
}

func TestGuestinfoCustomization_EmptyPasswordInjectsNothing(t *testing.T) {
	got, err := guestinfoCustomizationForInstance("linux", "", "host", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("empty password returned %d options, want nil", len(got))
	}
}

func TestGuestinfoCustomization_UnknownOSFailsClosed(t *testing.T) {
	if _, err := guestinfoCustomizationForInstance("freebsd", "Changeme123!", "host", "instance-1"); err == nil {
		t.Fatal("unknown OS accepted a password-bearing customization")
	}
}

func TestGuestinfoCustomization_MissingInstanceIDFailsClosed(t *testing.T) {
	if _, err := guestinfoCustomizationForInstance("linux", "Changeme123!", "host", ""); err == nil {
		t.Fatal("password-bearing customization accepted an empty instance ID")
	}
}

func TestGuestinfoCustomization_LinuxSetsDefaultUserPassword(t *testing.T) {
	opts, err := guestinfoCustomizationForInstance("linux", "Changeme123!", "tpl-ubuntu-test", "instance-linux-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 4 {
		t.Fatalf("linux produced %d options, want 4 (userdata + metadata + 2 encodings)", len(opts))
	}
	userdata, metadata := decodeGuestinfo(t, opts)

	if !strings.HasPrefix(userdata, "#cloud-config") {
		t.Fatalf("linux userdata is not a cloud-config document:\n%s", userdata)
	}
	if strings.Contains(userdata, "\npassword: Changeme123!\n") {
		t.Fatalf("linux userdata must not rely on the image default user:\n%s", userdata)
	}
	if !strings.Contains(userdata, "- name: student\n") ||
		!strings.Contains(userdata, `password: "Changeme123!"`) {
		t.Fatalf("linux userdata missing explicit student password mapping:\n%s", userdata)
	}

	if !strings.Contains(userdata, "expire: false") {
		t.Fatalf("linux userdata must not force password expiry:\n%s", userdata)
	}
	if !strings.Contains(userdata, `hostname: "tpl-ubuntu-test"`) {
		t.Fatalf("linux userdata missing hostname:\n%s", userdata)
	}
	if !strings.Contains(metadata, `"local-hostname": "tpl-ubuntu-test"`) {
		t.Fatalf("linux metadata missing local-hostname:\n%s", metadata)
	}
	if !strings.Contains(metadata, `"instance-id": "instance-linux-1"`) {
		t.Fatalf("linux metadata missing unique instance identity:\n%s", metadata)
	}
}

func TestGuestinfoCustomization_LinuxQuotesEveryLeadingSpecialCharacter(t *testing.T) {
	for _, special := range "!@#$%&*" {
		password := string(special) + "LeadingSafe1"
		t.Run(string(special), func(t *testing.T) {
			opts, err := guestinfoCustomizationForInstance(
				"linux",
				password,
				"safe-host",
				"safe-instance-"+string(special),
			)
			if err != nil {
				t.Fatal(err)
			}
			userdata, _ := decodeGuestinfo(t, opts)
			var config struct {
				Chpasswd struct {
					Users []struct {
						Name     string `yaml:"name"`
						Password string `yaml:"password"`
						Type     string `yaml:"type"`
					} `yaml:"users"`
				} `yaml:"chpasswd"`
			}
			if err := yaml.Unmarshal(
				[]byte(strings.TrimPrefix(userdata, "#cloud-config\n")),
				&config,
			); err != nil {
				t.Fatalf("cloud-config did not parse as YAML: %v\n%s", err, userdata)
			}
			if len(config.Chpasswd.Users) != 1 {
				t.Fatalf("chpasswd users = %d, want 1", len(config.Chpasswd.Users))
			}
			user := config.Chpasswd.Users[0]
			if user.Name != "student" || user.Password != password || user.Type != "text" {
				t.Fatalf("decoded chpasswd user = %+v, want explicit student and exact password", user)
			}
		})
	}
}

func TestGuestinfoCustomization_WindowsSetsStudentPassword(t *testing.T) {
	opts, err := guestinfoCustomizationForInstance("windows", "Changeme123!", "tpl-win-test", "instance-windows-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 4 {
		t.Fatalf("windows produced %d options, want 4", len(opts))
	}
	userdata, metadata := decodeGuestinfo(t, opts)

	if !strings.HasPrefix(userdata, "#ps1_sysnative") {
		t.Fatalf("windows userdata is not a cloudbase-init ps1 script:\n%s", userdata)
	}
	if !strings.Contains(userdata, "ConvertTo-SecureString 'Changeme123!'") {
		t.Fatalf("windows userdata missing password:\n%s", userdata)
	}
	if !strings.Contains(userdata, "Get-LocalUser -Name 'Student' | Set-LocalUser -Password $password") {
		t.Fatalf("windows userdata must set the Student account password:\n%s", userdata)
	}
	passwordCommand := "Get-LocalUser -Name 'Student' | " +
		"Set-LocalUser -Password " + "$password"
	if strings.Count(userdata, passwordCommand) != 1 || strings.Contains(userdata, "******") {
		t.Fatalf("windows userdata has malformed Student password wiring:\n%s", userdata)
	}
	if !strings.Contains(metadata, `"admin_pass": "Changeme123!"`) {
		t.Fatalf("windows metadata missing admin_pass:\n%s", metadata)
	}
	if !strings.Contains(metadata, `"instance-id": "instance-windows-1"`) {
		t.Fatalf("windows metadata missing unique instance identity:\n%s", metadata)
	}
}

func TestGuestinfoCustomization_InstanceIdentityIsNotHostname(t *testing.T) {
	first, err := guestinfoCustomizationForInstance("linux", "First1!", "reused-hostname", "clone-operation-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := guestinfoCustomizationForInstance("linux", "Second2!", "reused-hostname", "clone-operation-2")
	if err != nil {
		t.Fatal(err)
	}
	_, firstMetadata := decodeGuestinfo(t, first)
	_, secondMetadata := decodeGuestinfo(t, second)
	if firstMetadata == secondMetadata {
		t.Fatalf("distinct clone operations produced identical metadata: %s", firstMetadata)
	}
	if strings.Contains(firstMetadata, `"instance-id": "reused-hostname"`) ||
		strings.Contains(secondMetadata, `"instance-id": "reused-hostname"`) {
		t.Fatalf("hostname was reused as instance identity: first=%s second=%s", firstMetadata, secondMetadata)
	}
}
