// Package runnertools exposes the assessment runner's tool inventory to Go
// code.
//
// The inventory itself lives in tools.txt, which is also consumed by
// deploy/runner/Dockerfile to drive both the apt install list and the
// post-install verification loop. See the header of tools.txt for why a single
// source of truth matters here.
//
// The consumer that makes this worth having is the authoring-time script
// validator: it can now tell an instructor "gobuster is not installed in the
// runner image" while they are writing the action, instead of a student
// discovering it as a bare exit 127 during a graded assessment.
package runnertools

import (
	_ "embed"
	"sort"
	"strings"
)

//go:embed tools.txt
var manifestSrc string

// Entry is one line of the manifest.
type Entry struct {
	// Check is either a command name ("nmap") or an absolute filesystem path
	// ("/usr/share/seclists"). Paths are for packages that ship data rather
	// than an executable.
	Check string
	// Package is the apt package that provides Check.
	Package string
}

// IsPath reports whether this entry is verified with `test -e` rather than
// `command -v`.
func (e Entry) IsPath() bool { return strings.HasPrefix(e.Check, "/") }

var (
	entries  []Entry
	commands []string
	packages []string
	cmdSet   map[string]struct{}
)

func init() {
	seenPkg := make(map[string]struct{})
	cmdSet = make(map[string]struct{})

	for _, raw := range strings.Split(manifestSrc, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			// A malformed line is a manifest bug, not a runtime condition to
			// tolerate — but panicking in init() would take down the API for a
			// typo in a tool list. TestManifest_WellFormed fails the build
			// instead, which is where this belongs.
			continue
		}
		e := Entry{Check: f[0], Package: f[1]}
		entries = append(entries, e)
		if !e.IsPath() {
			if _, dup := cmdSet[e.Check]; !dup {
				cmdSet[e.Check] = struct{}{}
				commands = append(commands, e.Check)
			}
		}
		if _, dup := seenPkg[e.Package]; !dup {
			seenPkg[e.Package] = struct{}{}
			packages = append(packages, e.Package)
		}
	}
	sort.Strings(commands)
	sort.Strings(packages)
}

// Entries returns every manifest line, in file order.
func Entries() []Entry { return append([]Entry(nil), entries...) }

// Commands returns the sorted set of command names available in the runner
// image (manifest entries that are not filesystem paths).
func Commands() []string { return append([]string(nil), commands...) }

// Packages returns the sorted set of apt packages installed in the runner
// image.
func Packages() []string { return append([]string(nil), packages...) }

// HasCommand reports whether name is a command the runner image provides.
func HasCommand(name string) bool {
	_, ok := cmdSet[name]
	return ok
}

// HasPath reports whether p is a filesystem path the runner image provides
// (a manifest entry whose check starts with '/').
func HasPath(p string) bool {
	for _, e := range entries {
		if e.IsPath() && e.Check == p {
			return true
		}
	}
	return false
}

// RawManifest returns the embedded tools.txt exactly as committed. Used by the
// guard test that compares it against what the Dockerfile consumes.
func RawManifest() string { return manifestSrc }
