package opnsense

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestGetFirewallRules_ParsesFilterGetOptionMaps verifies GetFirewallRules reads
// the COMPLETE ruleset from firewall/filter/get and correctly decodes OPNsense's
// option-map shape for select fields (selected-key extraction, multi-select
// interface, numeric vs. string "selected"), plus the plain-string net/port
// fields. Regression guard for the 2026-08-02 incident where the reconciler
// deduped against searchRule (which returns only 1 row) and re-created rules.
func TestGetFirewallRules_ParsesFilterGetOptionMaps(t *testing.T) {
	// Two rules: a per-VLAN pod pass rule (single-interface, "selected" as int)
	// and the management rule (multi-interface lan+opt1, "selected" as string).
	const body = `{
	  "filter": {
	    "rules": {
	      "rule": {
	        "aaaa-pod": {
	          "enabled": "1",
	          "sequence": "1",
	          "interface": {
	            "opt3": {"value": "OPT3", "selected": 1},
	            "opt5": {"value": "OPT5", "selected": 0}
	          },
	          "direction": {
	            "in":  {"value": "in",  "selected": 1},
	            "out": {"value": "out", "selected": 0}
	          },
	          "action": {
	            "pass":  {"value": "Pass",  "selected": 1},
	            "block": {"value": "Block", "selected": 0}
	          },
	          "ipprotocol": {
	            "inet":  {"value": "IPv4", "selected": 1},
	            "inet6": {"value": "IPv6", "selected": 0}
	          },
	          "protocol": {
	            "any": {"value": "any", "selected": 1},
	            "TCP": {"value": "TCP", "selected": 0}
	          },
	          "source_net": "10.100.0.0/24",
	          "source_port": "",
	          "destination_net": "any",
	          "destination_port": "",
	          "description": ""
	        },
	        "bbbb-mgmt": {
	          "enabled": "1",
	          "sequence": "2",
	          "interface": {
	            "lan":  {"value": "LAN",  "selected": "1"},
	            "opt1": {"value": "OPT1", "selected": "1"},
	            "opt3": {"value": "OPT3", "selected": "0"}
	          },
	          "direction": {
	            "any": {"value": "any", "selected": "1"},
	            "in":  {"value": "in",  "selected": "0"}
	          },
	          "action": {
	            "pass":  {"value": "Pass",  "selected": "1"},
	            "block": {"value": "Block", "selected": "0"}
	          },
	          "ipprotocol": {
	            "inet": {"value": "IPv4", "selected": "1"}
	          },
	          "protocol": {
	            "any": {"value": "any", "selected": "1"}
	          },
	          "source_net": "10.10.10.0/24",
	          "source_port": "",
	          "destination_net": "10.100.0.0/16",
	          "destination_port": "",
	          "description": "Allow management VLAN to pod subnets"
	        }
	      }
	    }
	  }
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/firewall/filter/get" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, discardLogger())

	rules, err := c.GetFirewallRules(context.Background())
	if err != nil {
		t.Fatalf("GetFirewallRules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules from filter/get, got %d: %+v", len(rules), rules)
	}

	byUUID := map[string]FirewallRuleInfo{}
	for _, r := range rules {
		byUUID[r.UUID] = r
	}

	pod, ok := byUUID["aaaa-pod"]
	if !ok {
		t.Fatalf("pod rule not parsed; got %+v", rules)
	}
	if pod.Interface != "opt3" {
		t.Errorf("pod interface = %q, want %q", pod.Interface, "opt3")
	}
	if pod.Action != "pass" || pod.Direction != "in" || pod.IPProtocol != "inet" || pod.Protocol != "any" {
		t.Errorf("pod enum fields not canonicalized: %+v", pod)
	}
	if pod.Source != "10.100.0.0/24" || pod.Destination != "any" {
		t.Errorf("pod net fields = (%q,%q), want (10.100.0.0/24, any)", pod.Source, pod.Destination)
	}
	if pod.SourcePort != "" || pod.DestinationPort != "" {
		t.Errorf("pod ports should be empty, got (%q,%q)", pod.SourcePort, pod.DestinationPort)
	}

	mgmt, ok := byUUID["bbbb-mgmt"]
	if !ok {
		t.Fatalf("mgmt rule not parsed; got %+v", rules)
	}
	// Multi-select interface must be a sorted, canonical set — and the deselected
	// opt3 must be excluded.
	if mgmt.Interface != "lan,opt1" {
		t.Errorf("mgmt interface = %q, want %q (sorted set of selected)", mgmt.Interface, "lan,opt1")
	}
	if mgmt.Direction != "any" || mgmt.Action != "pass" {
		t.Errorf("mgmt enum fields (string 'selected') not parsed: %+v", mgmt)
	}
	if mgmt.Destination != "10.100.0.0/16" {
		t.Errorf("mgmt destination = %q, want 10.100.0.0/16", mgmt.Destination)
	}
}

func TestSelectedOptionSet_SortsAndFiltersSelected(t *testing.T) {
	m := map[string]opnOption{
		"opt5": {Value: "OPT5", Selected: []byte("1")},
		"lan":  {Value: "LAN", Selected: []byte(`"1"`)},
		"opt1": {Value: "OPT1", Selected: []byte("0")},
	}
	got := selectedOptionSet(m)
	if got != "lan,opt5" {
		t.Fatalf("selectedOptionSet = %q, want %q", got, "lan,opt5")
	}

	// Empty / all-deselected yields empty string.
	if s := selectedOptionSet(map[string]opnOption{"opt1": {Selected: []byte("0")}}); s != "" {
		t.Fatalf("expected empty selection, got %q", s)
	}
}
