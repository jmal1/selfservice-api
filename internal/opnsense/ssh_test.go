package opnsense

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveInterfaceScript_ReusesBeforeAllocating pins the ORDER of the two
// lookups. Reuse must come first: the network reconciler re-asserts desired
// state every 5 minutes, so if assignment allocates a fresh interface each pass
// it clobbers the previously-assigned pod's gateway, and the reconciler then
// "repairs" that one too -- a permanent loop that restarts Kea on every cycle
// and starves every pod of DHCP.
func TestResolveInterfaceScript_ReusesBeforeAllocating(t *testing.T) {
	script := resolveInterfaceScript("vlan03")

	reuse := strings.Index(script, "$iface['if'] === $vlan_dev")
	if reuse < 0 {
		t.Fatal("script never compares an interface against the VLAN device, so re-assignment cannot be idempotent")
	}
	alloc := strings.Index(script, "$used[intval(")
	if alloc < 0 {
		t.Fatal("script never builds the in-use set, so it cannot pick a free interface")
	}
	if reuse > alloc {
		t.Error("allocation happens before reuse: a re-assigned VLAN would get a NEW interface and clobber another pod's gateway")
	}
}

// TestResolveInterfaceScript_ScansNumerically guards the second half of the
// original bug. `sort -u | tail -1` is lexicographic, so with opt10 present it
// answers opt2 and the "next" interface is opt3 -- already in use.
func TestResolveInterfaceScript_ScansNumerically(t *testing.T) {
	script := resolveInterfaceScript("vlan03")

	if !strings.Contains(script, "intval($m[1])") {
		t.Error("interface index is not converted to an integer; string ordering puts opt10 before opt2")
	}
	if !strings.Contains(script, "for ($i = 1; $i <= 512; $i++)") {
		t.Error("expected a bounded ascending numeric scan for the lowest free interface")
	}
	if strings.Contains(script, "sort") || strings.Contains(script, "tail") {
		t.Error("shell sort/tail is back; that is what made this lexicographic in the first place")
	}
}

// TestResolveInterfaceScript_QuotesTheVLANDevice is a light injection guard.
// The script runs as root on the firewall.
func TestResolveInterfaceScript_QuotesTheVLANDevice(t *testing.T) {
	script := resolveInterfaceScript("vlan03")
	if !strings.Contains(script, "$vlan_dev = 'vlan03';") {
		t.Errorf("VLAN device not single-quoted into the script:\n%s", script)
	}
}

// TestVlanDevRe_RejectsShellAndPHPMetacharacters keeps the validation at the
// boundary meaningful. The value comes from OPNsense's own config today, but
// "it came from a trusted place" is not a property the compiler checks.
func TestVlanDevRe_RejectsShellAndPHPMetacharacters(t *testing.T) {
	good := []string{"vlan01", "vlan03", "vmx1_vlan10", "igb0.30"}
	for _, v := range good {
		if !vlanDevRe.MatchString(v) {
			t.Errorf("rejected a legitimate VLAN device %q", v)
		}
	}
	bad := []string{"vlan01'; system('rm -rf /'); '", "vlan01 vlan02", "vlan$(id)", "vlan01'", "", "../etc/passwd"}
	for _, v := range bad {
		if vlanDevRe.MatchString(v) {
			t.Errorf("accepted unsafe VLAN device %q", v)
		}
	}
}

// TestOptInterfaceRe_RejectsGarbage documents why resolveInterface validates
// what comes BACK. The predecessor treated any failure as "no interfaces exist"
// and returned opt1, so a broken command and an empty firewall were
// indistinguishable -- and every VLAN piled onto opt1.
func TestOptInterfaceRe_RejectsGarbage(t *testing.T) {
	for _, v := range []string{"opt1", "opt2", "opt512"} {
		if !optInterfaceRe.MatchString(v) {
			t.Errorf("rejected valid interface %q", v)
		}
	}
	for _, v := range []string{"", "lan", "wan", "opt", "<opt1>", "opt1\nopt2", "PHP Warning: ..."} {
		if optInterfaceRe.MatchString(v) {
			t.Errorf("accepted garbage %q as an interface name", v)
		}
	}
}

// TestNoGNUOnlyGrepFlags is the durable guard for the root cause.
//
// OPNsense runs FreeBSD, whose base grep is BSD grep. It has no -P (PCRE).
// `grep -oP ... /conf/config.xml` exited 2 on every single invocation, and
// because the caller collapsed "command failed" into "nothing found", it
// silently returned opt1 forever. Nothing in Go's toolchain can catch a
// portability bug inside a shell string, so it is caught here instead.
//
// If you need PCRE semantics on the firewall, do the work in PHP -- every other
// operation in this file already does, and PHP is guaranteed present.
//
// This walks the AST and inspects only string LITERALS. A plain text scan also
// matches doc comments, including the one above this test and the one on
// resolveInterfaceScript that quotes the offending command -- so it would fail
// on a correct file and get deleted for being noisy.
func TestNoGNUOnlyGrepFlags(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	// Flags BSD grep does not support.
	banned := []string{"grep -P", "grep -oP", "grep -Po", "grep --perl-regexp"}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		fset := token.NewFileSet()
		// Parse WITHOUT ParseComments so comments never enter the AST.
		file, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, b := range banned {
				if strings.Contains(lit.Value, b) {
					t.Errorf("%s:%d uses %q. OPNsense is FreeBSD: BSD grep has no -P, so this "+
						"command fails on every run and the caller cannot tell failure from "+
						"an empty result. Do the matching in PHP instead.",
						name, fset.Position(lit.Pos()).Line, b)
				}
			}
			return true
		})
	}

	// A guard that scans nothing passes for the wrong reason.
	if scanned == 0 {
		t.Fatal("scanned no source files; the guard is hollow")
	}
}
