package scriptvalidator

import "testing"

func TestDetectShellLanguage_Shebangs(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bash shebang", "#!/usr/bin/env bash\necho hi\n", "bash"},
		{"sh shebang", "#!/bin/sh\necho hi\n", "bash"},
		{"zsh shebang", "#!/usr/bin/env zsh\necho hi\n", "bash"},
		{"pwsh shebang", "#!/usr/bin/env pwsh\nGet-Service\n", "powershell"},
		{"powershell shebang", "#!/usr/bin/pwsh\n$x = 1\n", "powershell"},
		{"no shebang plain bash", "echo hi\nls -la\n", "bash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectShellLanguage(tc.in); got != tc.want {
				t.Errorf("detectShellLanguage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDetectShellLanguage_PowerShellHeuristics(t *testing.T) {
	// Two or more PS-flavoured constructs is enough to call it.
	ps := `$svc = Get-Service -Name "Spooler"
if ($svc.Status -ne "Running") {
    Write-Output "stopped"
}
`
	if got := detectShellLanguage(ps); got != "powershell" {
		t.Errorf("expected powershell for PS-shaped script, got %q", got)
	}

	// Just one $Var = "x" in an otherwise bash-shaped script shouldn't
	// trigger PS detection.
	bash := `local x="hello"
$Var = "embedded in a heredoc, not a real PS assignment"
echo "$x"
`
	if got := detectShellLanguage(bash); got != "bash" {
		t.Errorf("single PS-shaped line should not flip detection, got %q", got)
	}
}

func TestDetectShellLanguage_TypicalBashAction(t *testing.T) {
	// Lifted from the action corpus — uses local, [[ ]], ctx_set, etc.
	bash := `local cmd="" expect_output=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --cmd) cmd="$2"; shift 2;;
        *) shift;;
    esac
done
local output
output=$(eval "$cmd" 2>&1) || return 1
ctx_set "command_output" "$output"
return 0
`
	if got := detectShellLanguage(bash); got != "bash" {
		t.Errorf("typical bash action mis-detected as %q", got)
	}
}

func TestDetectShellLanguage_RealPSFromCorpus(t *testing.T) {
	// Lifted from a Windows action in the corpus.
	ps := `param(
    [string]$Service
)
$svc = Get-Service -Name $Service
if ($svc.Status -ne "Running") {
    Write-Output "Service $Service not running"
    exit 1
}
`
	if got := detectShellLanguage(ps); got != "powershell" {
		t.Errorf("corpus PS action mis-detected as %q", got)
	}
}
