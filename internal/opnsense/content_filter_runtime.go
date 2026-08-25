package opnsense

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const contentFilterDNSBLRuntimeDescriptionPrefix = "crucible:content-filter:v1:"

type dnsblRuntimeExpectation struct {
	UUID        string   `json:"uuid"`
	Description string   `json:"description"`
	SourceNets  []string `json:"source_nets"`
}

// ActivateUnboundDNSBL runs OPNsense's supported DNSBL action and independently
// validates the freshly generated runtime file. The action's own shell command
// ends in "|| true", so its HTTP/API status alone is not trustworthy.
func (c *Client) ActivateUnboundDNSBL(ctx context.Context, expected []DNSBLPolicy) error {
	sshClient, err := NewSSHClient(c.config, c.logger)
	if err != nil {
		return fmt.Errorf("activate Unbound DNSBL: %w", err)
	}
	return sshClient.activateUnboundDNSBL(ctx, expected, true)
}

// InspectUnboundDNSBLRuntime validates generated DNSBL state without refreshing
// feeds or mutating Unbound.
func (c *Client) InspectUnboundDNSBLRuntime(ctx context.Context, expected []DNSBLPolicy) error {
	sshClient, err := NewSSHClient(c.config, c.logger)
	if err != nil {
		return fmt.Errorf("inspect Unbound DNSBL runtime: %w", err)
	}
	return sshClient.activateUnboundDNSBL(ctx, expected, false)
}

func (s *SSHClient) activateUnboundDNSBL(ctx context.Context, expected []DNSBLPolicy, refresh bool) error {
	expectations, err := normalizeDNSBLRuntimeExpectations(expected)
	if err != nil {
		return err
	}
	expectationJSON, err := json.Marshal(expectations)
	if err != nil {
		return fmt.Errorf("marshal DNSBL runtime expectations: %w", err)
	}
	expectationPayload := base64.StdEncoding.EncodeToString(expectationJSON)
	validatorPayload := base64.StdEncoding.EncodeToString([]byte(dnsblRuntimeValidator))

	refreshCommand := ""
	if refresh {
		refreshCommand = `
before=$(/usr/bin/stat -f %m /var/unbound/data/dnsbl.json 2>/dev/null || printf 0)
/bin/sleep 1
output=$(/usr/local/sbin/configctl unbound dnsbl 2>&1)
rc=$?
[ "$rc" -eq 0 ] || { printf "%s\n" "$output" >&2; exit "$rc"; }
if printf "%s" "$output" | /usr/bin/grep -Eiq "failed|fatal|error|traceback"; then
  printf "masked DNSBL action failure: %s\n" "$output" >&2
  exit 1
fi
[ ! -e /var/unbound/data/dnsbl_format_warning ] || { printf "DNSBL format warning is present\n" >&2; exit 1; }
after=$(/usr/bin/stat -f %m /var/unbound/data/dnsbl.json 2>/dev/null || printf 0)
[ "$after" -gt "$before" ] || { printf "DNSBL runtime file was not freshly replaced\n" >&2; exit 1; }
`
	}
	command := fmt.Sprintf(`/bin/sh -c 'set -eu
%s
printf %%s %s | /usr/bin/base64 -d | /usr/local/bin/python3 - %s
status=$(/usr/local/sbin/configctl unbound status 2>&1)
printf %%s "$status" | /usr/bin/grep -qi "is running"
'`, refreshCommand, validatorPayload, expectationPayload)

	client, err := s.dial()
	if err != nil {
		return fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close()
	if _, err := s.runCommand(client, command); err != nil {
		if refresh {
			return fmt.Errorf("activate and verify Unbound DNSBL runtime: %w", err)
		}
		return fmt.Errorf("verify Unbound DNSBL runtime: %w", err)
	}
	return nil
}

func normalizeDNSBLRuntimeExpectations(policies []DNSBLPolicy) ([]dnsblRuntimeExpectation, error) {
	out := make([]dnsblRuntimeExpectation, 0, len(policies))
	seen := make(map[string]bool, len(policies))
	for _, policy := range policies {
		uuid := strings.TrimSpace(policy.UUID)
		description := strings.TrimSpace(policy.Description)
		if uuid == "" {
			return nil, fmt.Errorf("DNSBL runtime expectation omitted uuid")
		}
		if seen[uuid] {
			return nil, fmt.Errorf("duplicate DNSBL runtime expectation %q", uuid)
		}
		if !strings.HasPrefix(description, contentFilterDNSBLRuntimeDescriptionPrefix) {
			return nil, fmt.Errorf("DNSBL runtime expectation %q is not Crucible-owned", description)
		}
		var sources []string
		for _, value := range strings.Split(policy.SourceNets, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			prefix, err := netip.ParsePrefix(value)
			if err != nil || prefix.Masked().String() != value {
				return nil, fmt.Errorf("DNSBL runtime expectation has invalid source network %q", value)
			}
			sources = append(sources, value)
		}
		if len(sources) != 1 {
			return nil, fmt.Errorf("DNSBL runtime expectation %q must have exactly one source network", description)
		}
		sort.Strings(sources)
		seen[uuid] = true
		out = append(out, dnsblRuntimeExpectation{
			UUID:        uuid,
			Description: description,
			SourceNets:  sources,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
	return out, nil
}

const dnsblRuntimeValidator = `import json
import sys

expected = json.loads(__import__("base64").b64decode(sys.argv[1]).decode())
with open("/var/unbound/data/dnsbl.json", "r", encoding="utf-8") as handle:
    runtime = json.load(handle)

config = runtime.get("config")
data = runtime.get("data")
if not isinstance(config, dict) or not isinstance(data, dict):
    raise SystemExit("DNSBL runtime file omitted config or data")

prefix = "crucible:content-filter:v1:"
owned = {
    key: value
    for key, value in config.items()
    if isinstance(value, dict) and str(value.get("description", "")).startswith(prefix)
}
want = {item["uuid"]: item for item in expected}
if set(owned) != set(want):
    raise SystemExit("DNSBL runtime owned UUID set does not match desired model")

references = {key: 0 for key in want}
for metadata in data.values():
    if not isinstance(metadata, list):
        raise SystemExit("DNSBL runtime data metadata is malformed")
    for item in metadata:
        if isinstance(item, dict) and item.get("idx") in references:
            references[item["idx"]] += 1

for key, item in want.items():
    policy = owned[key]
    if policy.get("description") != item["description"]:
        raise SystemExit("DNSBL runtime description mismatch")
    if sorted(policy.get("source_nets", [])) != item["source_nets"]:
        raise SystemExit("DNSBL runtime source scope mismatch")
    if references[key] == 0:
        raise SystemExit("DNSBL runtime contains no domains for an owned policy")
`

type diagnosticRoute struct {
	Destination string `json:"destination"`
	Gateway     string `json:"gateway"`
	NetIf       string `json:"netif"`
}

type diagnosticInterface struct {
	IPv6 []struct {
		IPAddr string `json:"ipaddr"`
	} `json:"ipv6"`
}

// CheckStudentIPv6InternetRoute rejects a routed student IPv6 path while
// ignoring management/staging interfaces. Student interfaces are the exact
// active optN interfaces selected by the durable pod plan.
func (c *Client) CheckStudentIPv6InternetRoute(ctx context.Context, studentInterfaces []string) error {
	interfaces := make(map[string]bool, len(studentInterfaces))
	for _, value := range studentInterfaces {
		value = strings.TrimSpace(value)
		if value == "" || !strings.HasPrefix(value, "opt") {
			return fmt.Errorf("invalid student interface %q for IPv6 route inspection", value)
		}
		interfaces[value] = true
	}
	if len(interfaces) == 0 {
		return nil
	}

	routeBody, err := c.doRequest(ctx, "GET", "/diagnostics/interface/getRoutes", nil)
	if err != nil {
		return fmt.Errorf("get live route table: %w", err)
	}
	var routes []diagnosticRoute
	if err := json.Unmarshal(routeBody, &routes); err != nil {
		return fmt.Errorf("parse live route table: %w", err)
	}
	hasIPv6Default := false
	for _, route := range routes {
		destination := strings.ToLower(strings.TrimSpace(route.Destination))
		if destination != "::/0" && destination != "default" {
			continue
		}
		hasIPv6Default = true
		break
	}
	interfaceBody, err := c.doRequest(ctx, "GET", "/diagnostics/interface/getInterfaceConfig", nil)
	if err != nil {
		return fmt.Errorf("get live interface configuration: %w", err)
	}
	var live map[string]diagnosticInterface
	if err := json.Unmarshal(interfaceBody, &live); err != nil {
		return fmt.Errorf("parse live interface configuration: %w", err)
	}
	for name := range interfaces {
		current, exists := live[name]
		if !exists {
			return fmt.Errorf("student interface %s is absent from live interface configuration", name)
		}
		for _, addressField := range current.IPv6 {
			address, err := netip.ParseAddr(strings.TrimSpace(addressField.IPAddr))
			if err != nil {
				return fmt.Errorf("student interface %s has malformed IPv6 address %q", name, addressField.IPAddr)
			}
			if address.Is6() && !address.IsLinkLocalUnicast() {
				if hasIPv6Default {
					return fmt.Errorf("student interface %s has non-link-local IPv6 address %s while an IPv6 default route exists", name, address)
				}
				return fmt.Errorf("student interface %s has non-link-local IPv6 address %s; student IPv6 is unsupported", name, address)
			}
		}
	}
	return nil
}
