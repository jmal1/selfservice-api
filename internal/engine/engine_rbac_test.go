package engine

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestEngineChartGrantsNADUpdate is the guard for the fifth defect found while
// proving the runner end-to-end.
//
// ensureNAD reconciles the CNI config of an existing NetworkAttachmentDefinition
// rather than returning early on name match, so it issues an Update whenever the
// live config has drifted. The engine Role granted only create/get/list, so that
// Update failed with a 403 and every assessment run died at
// "provision runner: ensure NAD: update NAD: ... is forbidden".
//
// Code and chart are in different languages and different review paths, so this
// is precisely the class of mismatch nothing else catches: the Go tests pass
// against a fake client that has no RBAC, and helm lint has no idea which verbs
// the binary calls.
func TestEngineChartGrantsNADUpdate(t *testing.T) {
	verbs := nadVerbsFromChart(t)

	// Every verb here corresponds to a real call in k8s.go. Adding a verb to the
	// chart without a caller is over-permission; calling an API without the verb
	// is a runtime 403 that only appears in production.
	for _, required := range []string{"create", "get", "update"} {
		if !verbs[required] {
			t.Errorf("engine Role does not grant %q on network-attachment-definitions.\n"+
				"k8s.go calls Get, Create and Update on that resource. Without %q, "+
				"ensureNAD cannot correct a NAD whose CNI config has drifted, so every "+
				"run against an already-used VLAN fails with a 403 -- or, worse, if the "+
				"reconcile is removed to work around it, silently keeps a broken config "+
				"(the untagged-macvlan bug) while all tests stay green.\ngranted: %v",
				required, required, grantedVerbs(verbs))
		}
	}

	// Least privilege: the engine never deletes a NAD. NADs are keyed by VLAN and
	// reused across runs, so a delete grant would only widen blast radius.
	if verbs["delete"] {
		t.Error("engine Role grants delete on network-attachment-definitions, but no " +
			"code path deletes one. Remove it (least privilege) or add the caller.")
	}
}

// nadVerbsFromChart extracts the verbs granted on network-attachment-definitions
// from the engine Role. It parses the raw template rather than rendering the
// chart so the test needs no helm binary.
func nadVerbsFromChart(t *testing.T) map[string]bool {
	t.Helper()

	body, err := os.ReadFile(helmPath("templates", "engine-rbac.yaml"))
	if err != nil {
		t.Fatalf("read engine-rbac.yaml: %v", err)
	}
	text := string(body)

	idx := strings.Index(text, "network-attachment-definitions")
	if idx < 0 {
		t.Fatal("engine-rbac.yaml does not mention network-attachment-definitions at all; " +
			"the engine cannot attach runners to a pod VLAN without it")
	}

	// The verbs list is the first `verbs:` after the resource line.
	rest := text[idx:]
	m := regexp.MustCompile(`(?m)^\s*verbs:\s*\[([^\]]*)\]`).FindStringSubmatch(rest)
	if m == nil {
		t.Fatal("could not find a verbs: [...] list after the network-attachment-definitions " +
			"resource; if the chart moved to block-style verbs this test needs updating " +
			"rather than deleting")
	}

	verbs := map[string]bool{}
	for _, raw := range strings.Split(m[1], ",") {
		v := strings.Trim(strings.TrimSpace(raw), `"'`)
		if v != "" {
			verbs[v] = true
		}
	}
	if len(verbs) == 0 {
		t.Fatal("parsed an empty verb list -- the parser is broken, not the chart")
	}
	return verbs
}

func grantedVerbs(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
