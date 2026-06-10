package vcenter

import (
	"strings"
	"testing"
	"time"
)

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
