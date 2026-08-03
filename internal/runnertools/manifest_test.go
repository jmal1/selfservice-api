package runnertools_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/runnertools"
)

func TestManifest_WellFormed(t *testing.T) {
	entries := runnertools.Entries()
	if len(entries) < 20 {
		t.Fatalf("manifest parsed only %d entries; expected the full runner tool list", len(entries))
	}

	// Every non-comment, non-blank line must have produced exactly one entry.
	// A silently-dropped malformed line is the failure this guards: the Go side
	// would think a tool is unavailable (spurious CRU0002 warnings) while the
	// Dockerfile's awk still installs it, or vice versa.
	var wantLines int
	for _, raw := range strings.Split(runnertools.RawManifest(), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		wantLines++
		if len(strings.Fields(line)) < 2 {
			t.Errorf("malformed manifest line (needs '<check> <package>'): %q", line)
		}
	}
	if len(entries) != wantLines {
		t.Errorf("parsed %d entries from %d content lines; a line was dropped", len(entries), wantLines)
	}
}

func TestManifest_KeyToolsPresent(t *testing.T) {
	// These are the tools that justify the Kali base image at all. If one goes
	// missing, assessments silently stop being able to do the thing the base
	// image was chosen for.
	for _, cmd := range []string{"nmap", "netexec", "hydra", "smbclient", "nikto", "whatweb", "curl", "jq"} {
		if !runnertools.HasCommand(cmd) {
			t.Errorf("HasCommand(%q) = false; the runner image is supposed to provide it", cmd)
		}
	}
}

func TestManifest_RejectsAbsentTool(t *testing.T) {
	// The whole point of the manifest is telling truth about absence. If this
	// returned true for anything, CRU0002 would never fire.
	for _, cmd := range []string{"gobuster", "metasploit", "msfconsole", "sqlmap", ""} {
		if runnertools.HasCommand(cmd) {
			t.Errorf("HasCommand(%q) = true, but it is not in the manifest", cmd)
		}
	}
}

func TestManifest_PathEntriesAreNotCommands(t *testing.T) {
	// /usr/share/seclists is a data directory, not something a script can
	// invoke. Exposing it as a command would make `seclists` look callable.
	for _, e := range runnertools.Entries() {
		if e.IsPath() && runnertools.HasCommand(e.Check) {
			t.Errorf("path entry %q is exposed as a command", e.Check)
		}
	}
	found := false
	for _, e := range runnertools.Entries() {
		if e.Check == "/usr/share/seclists" && e.IsPath() {
			found = true
		}
	}
	if !found {
		t.Error("expected /usr/share/seclists to be present as a path entry")
	}
}

// TestDockerfile_DerivesToolsFromManifest is the guard that keeps the manifest
// a single source of truth rather than a fourth copy of the list.
//
// The Dockerfile previously spelled the inventory out twice — an apt list and a
// `command -v` loop — which could drift from each other and from anything the
// Go side believed. If someone reintroduces a hardcoded list, this fails.
func TestDockerfile_DerivesToolsFromManifest(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "deploy", "runner", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	// Strip comments before looking for instructions. The first version of this
	// test searched the raw file for "internal/runnertools/tools.txt" and was
	// therefore satisfied by the *comment* that explains the design -- deleting
	// the actual COPY left it green. Verified by deleting the COPY.
	var instructions []string
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		instructions = append(instructions, t)
	}
	df := strings.Join(instructions, "\n")

	if !strings.Contains(df, "COPY internal/runnertools/tools.txt") {
		t.Fatal("Dockerfile has no `COPY internal/runnertools/tools.txt` instruction; " +
			"the tool inventory must be derived from the manifest, not duplicated here")
	}
	if !strings.Contains(df, "/opt/crucible/tools.txt") {
		t.Error("Dockerfile does not read the manifest at /opt/crucible/tools.txt")
	}
	// The install step must consume the manifest rather than a literal list.
	if !strings.Contains(df, "awk") || !strings.Contains(df, "apt-get install") {
		t.Error("Dockerfile does not appear to derive the apt package list from the manifest")
	}

	// No package may ALSO appear as a hardcoded apt-get argument -- that would
	// be a second, drifting copy of the inventory.
	for _, pkg := range runnertools.Packages() {
		if strings.Contains(df, "\n"+pkg+" \\") {
			t.Errorf("package %q is hardcoded in the Dockerfile as well as listed in "+
				"tools.txt; remove the hardcoded copy so the manifest stays authoritative", pkg)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// docsAlwaysAvailable are commands the instructor docs may reference without a
// tools.txt entry because any Debian-family base image provides them (coreutils,
// shell builtins). They are not in the manifest because there is nothing to
// install and nothing that could plausibly go missing.
var docsAlwaysAvailable = map[string]bool{
	"awk": true, "sed": true, "grep": true, "cut": true, "sort": true,
	"tr": true, "head": true, "tail": true, "wc": true, "cat": true,
	"echo": true, "test": true, "printf": true, "tee": true, "xargs": true,
	"find": true, "base64": true, "date": true, "sleep": true, "env": true,
}

// docsToolCellRe pulls the "Tools" column out of the pre-installed-tools table
// in docs/instructor/runner-environment.md.
var docsToolRowRe = regexp.MustCompile(`(?m)^\|\s*[^|]+\|([^|]*)\|\s*$`)

// docsBacktickRe finds each `backticked` identifier in a table cell.
var docsBacktickRe = regexp.MustCompile("`([^`]+)`")

// TestInstructorDocs_ToolTableMatchesManifest asserts every tool advertised in
// the instructor wiki actually exists in the runner image.
//
// This table was a FOURTH copy of the tool inventory and had drifted badly: it
// advertised gobuster, wfuzz, hashcat, tcpdump, mtr, iperf3, httpie, yq and
// xmlstarlet -- none of which are installed -- plus two Crucible helper binaries
// (crucible-winrm, crucible-context) that do not exist at all.
//
// That drift is not cosmetic. An instructor reads this page, writes an action
// calling gobuster, ships it to a class, and every student's assessment fails at
// grading time with exit 127. The docs are the interface instructors program
// against, so they need the same guard as the Dockerfile.
//
// Verified by re-adding `gobuster` to the table and watching this fail.
func TestInstructorDocs_ToolTableMatchesManifest(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "docs", "instructor", "runner-environment.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Only scan the pre-installed tools section; the rest of the page has
	// prose and examples that legitimately mention other things.
	body := string(data)
	start := strings.Index(body, "## Pre-installed tools")
	if start < 0 {
		t.Fatal(`docs/instructor/runner-environment.md no longer has a ` +
			`"## Pre-installed tools" section; update this guard rather than deleting it`)
	}
	section := body[start:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}

	// The warning callout deliberately names tools that are NOT installed.
	// Scanning it would invert the assertion, so cut the section at it.
	if w := strings.Index(section, "> [!warning]"); w >= 0 {
		section = section[:w]
	}

	var checked int
	for _, row := range docsToolRowRe.FindAllStringSubmatch(section, -1) {
		for _, m := range docsBacktickRe.FindAllStringSubmatch(row[1], -1) {
			tool := strings.TrimSpace(m[1])
			// Absolute paths are verified as paths, not commands.
			if strings.HasPrefix(tool, "/") {
				checked++
				if !runnertools.HasPath(tool) && !strings.HasPrefix(tool, "/opt/crucible/") {
					t.Errorf("docs advertise path %q but tools.txt does not provide it", tool)
				}
				continue
			}
			checked++
			if runnertools.HasCommand(tool) || docsAlwaysAvailable[tool] {
				continue
			}
			t.Errorf("docs/instructor/runner-environment.md advertises %q in the "+
				"pre-installed tools table, but it is not in internal/runnertools/tools.txt "+
				"and is not a coreutils builtin.\n"+
				"An instructor who believes this page will ship an action that exits 127 "+
				"for every student.\n"+
				"Either add %q to tools.txt (and rebuild the image) or remove it from the table.",
				tool, tool)
		}
	}

	if checked == 0 {
		t.Fatal("parsed 0 tools from the docs table; the parse is broken, so this " +
			"guard would pass no matter how badly the docs drifted")
	}
}

// TestManifest_NoCarriageReturns asserts tools.txt uses LF line endings.
//
// This is not style pedantry. The Dockerfile parses this file with awk and with
// `while read -r check pkg _rest`, both running on Linux. With CRLF, awk's $2 for
// "bash  bash\r" is "bash\r", so the build runs `apt-get install bash\r` and fails
// -- or worse, a check name carries a trailing \r and `command -v` reports a tool
// missing that is actually installed.
//
// Go's strings.Fields treats \r as whitespace, so the entire Go test suite passes
// against a CRLF manifest while the image build is broken. That asymmetry is
// exactly why this needs its own assertion: editing this file on Windows converts
// it silently and nothing else in the repo notices.
func TestManifest_NoCarriageReturns(t *testing.T) {
	if i := strings.IndexByte(runnertools.RawManifest(), '\r'); i >= 0 {
		line := 1 + strings.Count(runnertools.RawManifest()[:i], "\n")
		t.Fatalf("internal/runnertools/tools.txt contains a carriage return at line %d; "+
			"it must use LF endings because the Dockerfile parses it with awk and "+
			"`read` on Linux. Convert the file to LF.", line)
	}
}
