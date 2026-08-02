package unattend

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const testPlaintextPassword = "PlAinTextPassw0rd-DO-NOT-LEAK!"

// --- cloudinit_cidata -------------------------------------------------------

func cidataUserData(t *testing.T, s Spec) ([]byte, []byte) {
	t.Helper()
	if s.Mode == "" {
		s.Mode = ModeCloudInitCIData
	}
	_, iso, err := BuildSeedISO(s)
	if err != nil {
		t.Fatalf("BuildSeedISO returned error: %v", err)
	}
	files, err := isoReadRootFiles(iso)
	if err != nil {
		t.Fatalf("reading ISO root: %v", err)
	}
	ud, ok := files["user-data"]
	if !ok {
		t.Fatalf("user-data not found in ISO root; have: %v", keysOf(files))
	}
	if _, ok := files["meta-data"]; !ok {
		t.Fatalf("meta-data file missing from ISO root; have: %v", keysOf(files))
	}
	return iso, ud
}

func TestBuildSeedISO_CIData_UserDataParsesAsYAML(t *testing.T) {
	_, ud := cidataUserData(t, Spec{
		Mode:     ModeCloudInitCIData,
		Hostname: "ubuntu-lab",
		Password: testPlaintextPassword,
	})

	if !strings.HasPrefix(string(ud), "#cloud-config\n") {
		t.Errorf("user-data must begin with the literal #cloud-config line, got: %q", firstLine(ud))
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal(ud, &doc); err != nil {
		t.Fatalf("user-data does not parse as YAML: %v", err)
	}
	if _, ok := doc["autoinstall"]; !ok {
		t.Errorf("user-data YAML is missing the autoinstall key; keys=%v", keysOfAny(doc))
	}
}

func TestBuildSeedISO_CIData_VolumeLabel(t *testing.T) {
	iso, _ := cidataUserData(t, Spec{Password: testPlaintextPassword})
	label, err := isoLabel(iso)
	if err != nil {
		t.Fatalf("reading ISO label: %v", err)
	}
	if label != "CIDATA" {
		t.Errorf("volume label = %q, want exactly CIDATA", label)
	}
}

func TestBuildSeedISO_CIData_DefaultUserIsStudent(t *testing.T) {
	for _, name := range []string{"empty username", "explicit blank"} {
		t.Run(name, func(t *testing.T) {
			_, ud := cidataUserData(t, Spec{
				Mode:     ModeCloudInitCIData,
				Username: "", // must default to student
				Password: testPlaintextPassword,
			})
			username := identityUsername(t, ud)
			if username != "student" {
				t.Errorf("identity.username = %q, want student", username)
			}
		})
	}
}

func TestBuildSeedISO_CIData_NoPlaintextPassword(t *testing.T) {
	iso, ud := cidataUserData(t, Spec{Password: testPlaintextPassword})

	if bytes.Contains(iso, []byte(testPlaintextPassword)) {
		t.Errorf("plaintext password leaked into ISO bytes")
	}
	if !bytes.Contains(ud, []byte("$6$")) {
		t.Errorf("user-data does not contain a $6$ SHA-512 crypt hash")
	}
	// The stored password must be a crypt hash, not the plaintext.
	pw := identityPassword(t, ud)
	if !strings.HasPrefix(pw, "$6$") {
		t.Errorf("identity.password = %q, want a $6$ hash", pw)
	}
}

// --- windows_autounattend ---------------------------------------------------

func autounattendXML(t *testing.T, s Spec) ([]byte, []byte) {
	t.Helper()
	s.Mode = ModeWindowsAutounattend
	_, iso, err := BuildSeedISO(s)
	if err != nil {
		t.Fatalf("BuildSeedISO returned error: %v", err)
	}
	files, err := isoReadRootFiles(iso)
	if err != nil {
		t.Fatalf("reading ISO root: %v", err)
	}
	xmlBytes, ok := files["autounattend.xml"]
	if !ok {
		t.Fatalf("autounattend.xml not at ISO root; have: %v", keysOf(files))
	}
	return iso, xmlBytes
}

func TestBuildSeedISO_Autounattend_ParsesAsXML(t *testing.T) {
	_, xmlBytes := autounattendXML(t, Spec{Hostname: "winlab", Password: testPlaintextPassword})

	// Well-formedness: decode every token to EOF.
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("autounattend.xml is not well-formed XML: %v", err)
		}
	}
}

func TestBuildSeedISO_Autounattend_NoPlaintextPassword(t *testing.T) {
	iso, xmlBytes := autounattendXML(t, Spec{Password: testPlaintextPassword})

	if bytes.Contains(iso, []byte(testPlaintextPassword)) {
		t.Errorf("plaintext password leaked into ISO bytes")
	}
	if !bytes.Contains(xmlBytes, []byte("<PlainText>false</PlainText>")) {
		t.Errorf("autounattend.xml must set <PlainText>false</PlainText>")
	}
	if bytes.Contains(xmlBytes, []byte("<PlainText>true</PlainText>")) {
		t.Errorf("autounattend.xml must not store a plaintext password")
	}
}

func TestEncodeWindowsPassword_MatchesRepoVector(t *testing.T) {
	// Regression vector from internal/provisioner/assets/windows-unattend.xml:
	// base64(UTF-16LE("Changeme123!" + "Password")).
	const want = "QwBoAGEAbgBnAGUAbQBlADEAMgAzACEAUABhAHMAcwB3AG8AcgBkAA=="
	got := encodeWindowsPassword("Changeme123!", "Password")
	if got != want {
		t.Errorf("encodeWindowsPassword mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// --- debian_preseed ---------------------------------------------------------

func TestBuildPreseed_InjectsBootParams(t *testing.T) {
	// A representative isolinux txt.cfg entry with a trailing "---".
	const isolinux = `default install
label install
	menu label ^Install
	kernel /install.amd/vmlinuz
	append vga=788 initrd=/install.amd/initrd.gz --- quiet
`
	patched := PatchBootloaderConfig(isolinux)
	if !strings.Contains(patched, preseedBootParams) {
		t.Fatalf("patched isolinux config missing boot params %q:\n%s", preseedBootParams, patched)
	}

	// GRUB linux line without a separator.
	const grub = `menuentry "Install" {
	set background_color=black
	linux /install.amd/vmlinuz vga=788
	initrd /install.amd/initrd.gz
}
`
	patchedGrub := PatchBootloaderConfig(grub)
	if !strings.Contains(patchedGrub, preseedBootParams) {
		t.Fatalf("patched grub config missing boot params %q:\n%s", preseedBootParams, patchedGrub)
	}

	// Idempotence: patching again must not duplicate the params.
	if again := PatchBootloaderConfig(patched); strings.Count(again, preseedBootParams) != strings.Count(patched, preseedBootParams) {
		t.Errorf("PatchBootloaderConfig is not idempotent")
	}
}

func TestBuildPreseed_ContentContract(t *testing.T) {
	s := Spec{
		Mode:      ModeDebianPreseed,
		Hostname:  "kali-lab",
		Password:  testPlaintextPassword,
		AptProxy:  "http://10.10.30.20:3142",
		ExtraPkgs: []string{"kali-linux-headless"},
	}
	cfg, err := BuildPreseedConfig(s)
	if err != nil {
		t.Fatalf("BuildPreseedConfig error: %v", err)
	}

	mustContain := []string{
		"d-i passwd/username string student",
		"$6$",
		"open-vm-tools",
		"cloud-init",
		"kali-linux-headless",
		"http://10.10.30.20:3142",
		"d-i finish-install/reboot_in_progress note",
		"d-i debian-installer/locale string en_US.UTF-8",
		"d-i time/zone string America/New_York",
	}
	for _, want := range mustContain {
		if !strings.Contains(cfg, want) {
			t.Errorf("preseed.cfg missing %q\n---\n%s", want, cfg)
		}
	}

	if strings.Contains(cfg, testPlaintextPassword) {
		t.Errorf("preseed.cfg leaked plaintext password")
	}

	// Without an apt proxy the mirror proxy line must be absent.
	noProxy, err := BuildPreseedConfig(Spec{Mode: ModeDebianPreseed, Password: testPlaintextPassword})
	if err != nil {
		t.Fatalf("BuildPreseedConfig (no proxy) error: %v", err)
	}
	if strings.Contains(noProxy, "mirror/http/proxy") {
		t.Errorf("preseed.cfg must not set a proxy when AptProxy is empty")
	}
}

func TestRemasterPreseedISO_ReturnsUnsupported(t *testing.T) {
	// Build a small valid ISO to feed as the "installer" so we exercise the
	// readable-but-unsupported path rather than a read error.
	iso, err := buildISO([]isoFile{{Path: "readme", Data: []byte("x")}}, "INSTALLER")
	if err != nil {
		t.Fatalf("building test ISO: %v", err)
	}
	data, err := RemasterPreseedISO(bytes.NewReader(iso), int64(len(iso)), Spec{
		Mode:     ModeDebianPreseed,
		Password: testPlaintextPassword,
	})
	if data != nil {
		t.Errorf("expected nil data on unsupported remaster, got %d bytes", len(data))
	}
	if !errors.Is(err, ErrRemasterUnsupported) {
		t.Errorf("expected ErrRemasterUnsupported, got %v", err)
	}
}

// --- dispatch & spec --------------------------------------------------------

func TestBuildSeedISO_RejectsUnknownMode(t *testing.T) {
	_, _, err := BuildSeedISO(Spec{Mode: "totally_bogus", Password: testPlaintextPassword})
	if !errors.Is(err, ErrUnknownMode) {
		t.Errorf("unknown mode: expected ErrUnknownMode, got %v", err)
	}
	if errors.Is(err, ErrManualMode) {
		t.Errorf("unknown mode must not be reported as manual mode")
	}
}

func TestBuildSeedISO_RejectsManualMode(t *testing.T) {
	_, _, err := BuildSeedISO(Spec{Mode: ModeManual, Password: testPlaintextPassword})
	if !errors.Is(err, ErrManualMode) {
		t.Errorf("manual mode: expected ErrManualMode, got %v", err)
	}
}

func TestBuildSeedISO_DebianPreseedIsNotASeedISO(t *testing.T) {
	_, _, err := BuildSeedISO(Spec{Mode: ModeDebianPreseed, Password: testPlaintextPassword})
	if !errors.Is(err, ErrPreseedRequiresRemaster) {
		t.Errorf("debian_preseed: expected ErrPreseedRequiresRemaster, got %v", err)
	}
}

func TestSpec_StringRedactsPassword(t *testing.T) {
	s := Spec{
		Mode:     ModeCloudInitCIData,
		Hostname: "h",
		Username: "student",
		Password: testPlaintextPassword,
	}
	if got := s.String(); strings.Contains(got, testPlaintextPassword) {
		t.Errorf("Spec.String() leaked password: %s", got)
	}
	if got := fmt.Sprintf("%v", s); strings.Contains(got, testPlaintextPassword) {
		t.Errorf("fmt %%v leaked password: %s", got)
	}
	if got := fmt.Sprintf("%s", s); strings.Contains(got, testPlaintextPassword) {
		t.Errorf("fmt %%s leaked password: %s", got)
	}
	// Pointer form should also be safe (fmt uses the value method set).
	if got := fmt.Sprintf("%v", &s); strings.Contains(got, testPlaintextPassword) {
		t.Errorf("fmt %%v of *Spec leaked password: %s", got)
	}
}

func TestSpec_Defaults(t *testing.T) {
	got := Spec{}.withDefaults()
	if got.Username != "student" {
		t.Errorf("default Username = %q, want student", got.Username)
	}
	if got.Locale != "en_US.UTF-8" {
		t.Errorf("default Locale = %q, want en_US.UTF-8", got.Locale)
	}
	if got.TimeZone != "America/New_York" {
		t.Errorf("default TimeZone = %q, want America/New_York", got.TimeZone)
	}

	// Explicit values must be preserved.
	custom := Spec{Username: "ops", Locale: "de_DE.UTF-8", TimeZone: "UTC"}.withDefaults()
	if custom.Username != "ops" || custom.Locale != "de_DE.UTF-8" || custom.TimeZone != "UTC" {
		t.Errorf("withDefaults overrode explicit values: %+v", custom)
	}
}

// --- helpers ----------------------------------------------------------------

func identityMap(t *testing.T, userData []byte) map[string]interface{} {
	t.Helper()
	var doc map[string]interface{}
	if err := yaml.Unmarshal(userData, &doc); err != nil {
		t.Fatalf("user-data does not parse as YAML: %v", err)
	}
	ai, ok := doc["autoinstall"].(map[string]interface{})
	if !ok {
		t.Fatalf("autoinstall block missing or wrong type: %T", doc["autoinstall"])
	}
	id, ok := ai["identity"].(map[string]interface{})
	if !ok {
		t.Fatalf("identity block missing or wrong type: %T", ai["identity"])
	}
	return id
}

func identityUsername(t *testing.T, userData []byte) string {
	t.Helper()
	u, _ := identityMap(t, userData)["username"].(string)
	return u
}

func identityPassword(t *testing.T, userData []byte) string {
	t.Helper()
	p, _ := identityMap(t, userData)["password"].(string)
	return p
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfAny(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
