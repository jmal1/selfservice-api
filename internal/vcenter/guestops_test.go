package vcenter

import (
	"strings"
	"testing"
	"time"
)

func TestDecodeGuestOutput(t *testing.T) {
	// UTF-16LE with BOM — exactly what Windows PowerShell 5.1 `*>`/`>`
	// redirection writes; this is what defeated the BitLocker sentinel.
	utf16le := []byte{0xFF, 0xFE}
	for _, r := range "BL:DECRYPTED\r\n" {
		utf16le = append(utf16le, byte(r), 0x00)
	}
	// UTF-16BE with BOM.
	utf16be := []byte{0xFE, 0xFF}
	for _, r := range "BL:NONE" {
		utf16be = append(utf16be, 0x00, byte(r))
	}
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"utf16le_bom", utf16le, "BL:DECRYPTED\r\n"},
		{"utf16be_bom", utf16be, "BL:NONE"},
		{"utf8_bom", append([]byte{0xEF, 0xBB, 0xBF}, []byte("hi")...), "hi"},
		{"plain_utf8", []byte("plain output"), "plain output"},
		{"empty", []byte{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeGuestOutput(tc.in); got != tc.want {
				t.Errorf("decodeGuestOutput(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// Regression: the sentinel match must succeed on decoded UTF-16 output.
	if !strings.Contains(decodeGuestOutput(utf16le), "BL:DECRYPTED") {
		t.Error("decoded UTF-16LE output must contain the BL:DECRYPTED sentinel")
	}
}

func TestValidateGuestRequest_HappyPath(t *testing.T) {
	req := GuestExecRequest{
		VMMoref:       "vm-1234",
		GuestUser:     "student",
		GuestPassword: "hunter2",
		Language:      "bash",
		Script:        "echo hi",
		RunID:         "11111111-1111-1111-1111-111111111111",
		ActionSlug:    "check-port",
	}
	if err := validateGuestRequest(req); err != nil {
		t.Fatalf("happy path should pass, got: %v", err)
	}
}

func TestValidateGuestRequest_RejectsEmpties(t *testing.T) {
	base := GuestExecRequest{
		VMMoref: "vm-1", GuestUser: "u", GuestPassword: "p",
		Language: "bash", Script: "echo x",
		RunID: "r", ActionSlug: "a",
	}
	cases := map[string]GuestExecRequest{
		"empty moref":      func() GuestExecRequest { r := base; r.VMMoref = ""; return r }(),
		"empty user":       func() GuestExecRequest { r := base; r.GuestUser = ""; return r }(),
		"empty pass":       func() GuestExecRequest { r := base; r.GuestPassword = ""; return r }(),
		"empty runid":      func() GuestExecRequest { r := base; r.RunID = ""; return r }(),
		"empty actionslug": func() GuestExecRequest { r := base; r.ActionSlug = ""; return r }(),
		"empty script":     func() GuestExecRequest { r := base; r.Script = ""; return r }(),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateGuestRequest(req); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

func TestValidateGuestRequest_RejectsOversizedScript(t *testing.T) {
	req := GuestExecRequest{
		VMMoref: "vm-1", GuestUser: "u", GuestPassword: "p",
		Language: "bash",
		Script:   strings.Repeat("a", maxGuestScriptBytes+1),
		RunID:    "r", ActionSlug: "a",
	}
	if err := validateGuestRequest(req); err == nil {
		t.Error("expected oversize script to fail validation")
	}
}

func TestValidateGuestRequest_RejectsUnknownLanguage(t *testing.T) {
	req := GuestExecRequest{
		VMMoref: "vm-1", GuestUser: "u", GuestPassword: "p",
		Language: "python",
		Script:   "print('hi')",
		RunID:    "r", ActionSlug: "a",
	}
	if err := validateGuestRequest(req); err == nil {
		t.Error("expected unknown language to fail validation")
	}
}

func TestNormalizeGuestTimeout_Defaults(t *testing.T) {
	if got := normalizeGuestTimeout(0); got != defaultGuestTimeout {
		t.Errorf("zero → default, got %v", got)
	}
	if got := normalizeGuestTimeout(-1 * time.Minute); got != defaultGuestTimeout {
		t.Errorf("negative → default, got %v", got)
	}
}

func TestNormalizeGuestTimeout_CapsAtMax(t *testing.T) {
	if got := normalizeGuestTimeout(1 * time.Hour); got != maxGuestTimeout {
		t.Errorf("1h → maxGuestTimeout, got %v", got)
	}
}

func TestNormalizeGuestTimeout_PreservesValidValues(t *testing.T) {
	in := 90 * time.Second
	if got := normalizeGuestTimeout(in); got != in {
		t.Errorf("valid timeout passed through, got %v", got)
	}
}

func TestScriptSuffix_KnownLanguages(t *testing.T) {
	cases := map[string]string{
		"bash":       ".sh",
		"sh":         ".sh",
		"shell":     ".sh",
		"powershell": ".ps1",
		"pwsh":       ".ps1",
		"POWERSHELL": ".ps1",
		"unknown":    ".sh", // fallback
	}
	for lang, want := range cases {
		if got := scriptSuffix(lang); got != want {
			t.Errorf("scriptSuffix(%q) = %q, want %q", lang, got, want)
		}
	}
}

func TestBuildGuestInvocation_BashRedirectsBothStreams(t *testing.T) {
	prog, args := buildGuestInvocation("bash", "/tmp/x.sh", "/tmp/x.stdout", "/tmp/x.stderr")
	if prog != "/bin/sh" {
		t.Errorf("expected /bin/sh, got %q", prog)
	}
	if !strings.Contains(args, "/tmp/x.sh") {
		t.Errorf("args missing script path: %q", args)
	}
	if !strings.Contains(args, "> \"/tmp/x.stdout\"") {
		t.Errorf("args missing stdout redirect: %q", args)
	}
	if !strings.Contains(args, "2> \"/tmp/x.stderr\"") {
		t.Errorf("args missing stderr redirect: %q", args)
	}
}

func TestBuildGuestInvocation_PowerShellUsesExecutionPolicyBypass(t *testing.T) {
	prog, args := buildGuestInvocation("powershell", `C:\Temp\x.ps1`, `C:\Temp\x.out`, `C:\Temp\x.err`)
	if !strings.HasSuffix(prog, "powershell.exe") {
		t.Errorf("expected powershell.exe binary, got %q", prog)
	}
	if !strings.Contains(args, "-ExecutionPolicy Bypass") {
		t.Errorf("expected -ExecutionPolicy Bypass to skip signing requirements, got %q", args)
	}
	if !strings.Contains(args, "-NonInteractive") {
		t.Errorf("expected -NonInteractive flag, got %q", args)
	}
	if !strings.Contains(args, "-NoProfile") {
		t.Errorf("expected -NoProfile flag, got %q", args)
	}
	// Regression: must use -Command (not -File) so PowerShell parses and
	// applies the stream redirection. With -File, the redirection tokens
	// are passed as script arguments and the .stdout file is never
	// written, so every PowerShell guest step read empty output and hung.
	if !strings.Contains(args, "-Command") {
		t.Errorf("expected -Command so redirection is parsed by PowerShell, got %q", args)
	}
	if strings.Contains(args, "-File") {
		t.Errorf("-File must NOT be used: it turns redirection tokens into script args, got %q", args)
	}
	if !strings.Contains(args, `*> 'C:\Temp\x.out'`) {
		t.Errorf("args missing all-stream redirect to stdout path: %q", args)
	}
	if !strings.Contains(args, `2> 'C:\Temp\x.err'`) {
		t.Errorf("args missing stderr redirect: %q", args)
	}
	if !strings.Contains(args, `& 'C:\Temp\x.ps1'`) {
		t.Errorf("args missing call-operator invocation of the script: %q", args)
	}
}

// TestGuestTempBase covers the OS-appropriate temp path resolution that
// historically was hardcoded to /tmp/... and broke every Windows generalize
// attempt with "Permission to perform this operation was denied" because
// VMware Tools resolved /tmp on Windows to C:\tmp which (a) does not exist
// and (b) the root of C: requires elevation that VMware Tools'' interactive
// logon with a UAC-filtered admin token does not have.
func TestGuestTempBase(t *testing.T) {
	cases := []struct {
		name      string
		language  string
		guestUser string
		runID     string
		slug      string
		want      string
		wantErr   bool
	}{
		{
			name:      "linux bash uses /tmp regardless of user",
			language:  "bash",
			guestUser: "anyone",
			runID:     "run-1",
			slug:      "slug-a",
			want:      "/tmp/crucible-run-1-slug-a",
		},
		{
			name:      "linux unknown language defaults to /tmp",
			language:  "",
			guestUser: "anyone",
			runID:     "run-1",
			slug:      "slug-a",
			want:      "/tmp/crucible-run-1-slug-a",
		},
		{
			name:      "windows powershell uses user profile temp",
			language:  "powershell",
			guestUser: "Student",
			runID:     "run-1",
			slug:      "slug-a",
			want:      `C:\Users\Student\AppData\Local\Temp\crucible-run-1-slug-a`,
		},
		{
			name:      "windows pwsh alias is recognized",
			language:  "pwsh",
			guestUser: "Student",
			runID:     "r",
			slug:      "s",
			want:      `C:\Users\Student\AppData\Local\Temp\crucible-r-s`,
		},
		{
			name:      "windows requires guestUser to construct the path",
			language:  "powershell",
			guestUser: "",
			wantErr:   true,
		},
		{
			name:      "windows rejects path-traversal in guestUser",
			language:  "powershell",
			guestUser: `..\Administrator`,
			wantErr:   true,
		},
		{
			name:      "windows rejects domain-prefixed user (local accounts only)",
			language:  "powershell",
			guestUser: `LAB\Student`,
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := guestTempBase(tc.language, tc.guestUser, tc.runID, tc.slug)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("guestTempBase() = %q; want %q", got, tc.want)
			}
		})
	}
}
