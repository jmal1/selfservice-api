package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ActionLibraryPath is where the generated action library is written and where
// /opt/crucible/lib/actions.sh sources it from. Both ends of that contract are
// asserted by TestActionsSh_SourcesGeneratedLibraryPath.
const ActionLibraryPath = "/opt/crucible/lib/library.sh"

// MaterializeActionLibrary validates the engine-generated action library and,
// if it is sound, writes it where actions.sh will source it.
//
// The library is delivered per run rather than baked into the image because the
// bodies live in the database and instructors edit them; an image-baked copy
// would silently drift from what the authoring UI validates against.
//
// Validation happens BEFORE the file is written, and that ordering is the point.
// The library is sourced as a single file, so one malformed body does not fail
// one action -- it fails `source` and therefore every action in the run, with an
// error that points at actions.sh rather than at the offending entry. Checking
// first means a bad library can never be left on disk to poison a run, and the
// failure is one precise, attributable error before any workflow starts.
//
// An empty library is not an error: a deployment with no library actions is
// unusual but legitimate, and workflows that call none of them still work.
func MaterializeActionLibrary(content string) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}

	if err := ValidateBashSyntax(content); err != nil {
		return fmt.Errorf("generated action library is not valid bash: %w", err)
	}

	dir := filepath.Dir(ActionLibraryPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create action library dir %s: %w", dir, err)
	}
	if err := os.WriteFile(ActionLibraryPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write action library %s: %w", ActionLibraryPath, err)
	}
	return nil
}

// ValidateBashSyntax parses a shell script with `bash -n` without executing it.
//
// The script is fed on stdin rather than written to a temp file. That avoids
// handing bash a native path, which matters because on Windows developer
// machines git-bash mangles `C:\Users\...` into a path it cannot open and the
// check would fail with exit 127 on perfectly valid input -- a false positive
// that looks exactly like a real syntax error. bash still reports line numbers
// for stdin, so diagnostics are unaffected.
func ValidateBashSyntax(content string) error {
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
