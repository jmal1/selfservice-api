package opnsense

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Config holds OPNsense connection settings.
type Config struct {
	BaseURL     string // e.g., "https://10.10.10.60/api"
	APIKey      string
	APISecret   string
	SSHHost     string // e.g., "10.10.10.60:22"
	SSHUser     string // e.g., "root"
	SSHKey      []byte // private key PEM (or password-based via SSHPassword)
	SSHPassword string
}

// VLAN represents an OPNsense VLAN interface.
type VLAN struct {
	UUID      string `json:"uuid"`
	Interface string `json:"if"`
	Tag       string `json:"tag"`
	Descr     string `json:"descr"`
	VLANIf    string `json:"vlanif"` // actual kernel device name (e.g., "vlan01")
}

// DHCPSubnet represents a Kea DHCP v4 subnet.
type DHCPSubnet struct {
	UUID    string `json:"uuid"`
	Subnet  string `json:"subnet"`
	Pools   string `json:"pools"`
	Gateway string `json:"option_data_autocollect"`
}

// Client provides REST API and SSH access to OPNsense.
type Client struct {
	config     Config
	httpClient *http.Client
	logger     *slog.Logger
}

// New creates an OPNsense client.
func New(cfg Config, logger *slog.Logger) *Client {
	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // OPNsense uses self-signed cert
				},
			},
		},
		logger: logger,
	}
}

// doRequest executes an authenticated API request with retry.
func (c *Client) doRequest(ctx context.Context, method, path string, body any) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			c.logger.Info("retrying OPNsense API call", "path", path, "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		var bodyReader io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("marshal request: %w", err)
			}
			bodyReader = bytes.NewReader(data)
		}

		url := c.config.BaseURL + path
		req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		req.SetBasicAuth(c.config.APIKey, c.config.APISecret)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response: %w", err)
			continue
		}

		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
			if resp.StatusCode >= 500 {
				continue // retry on server errors
			}
			return nil, lastErr // don't retry client errors
		}

		return respBody, nil
	}

	return nil, fmt.Errorf("all retries exhausted: %w", lastErr)
}

// ---------- VLAN Operations ----------

// CreateVLAN creates a VLAN on the specified parent interface.
func (c *Client) CreateVLAN(ctx context.Context, parentIf string, tag int, descr string) (string, error) {
	payload := map[string]any{
		"vlan": map[string]any{
			"if":    parentIf,
			"tag":   fmt.Sprintf("%d", tag),
			"descr": descr,
		},
	}

	resp, err := c.doRequest(ctx, "POST", "/interfaces/vlan_settings/addItem", payload)
	if err != nil {
		return "", fmt.Errorf("create VLAN %d: %w", tag, err)
	}

	var result struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("parse VLAN response: %w", err)
	}

	c.logger.Info("created VLAN", "tag", tag, "uuid", result.UUID)
	return result.UUID, nil
}

// DeleteVLAN removes a VLAN by UUID.
func (c *Client) DeleteVLAN(ctx context.Context, uuid string) error {
	_, err := c.doRequest(ctx, "POST", "/interfaces/vlan_settings/delItem/"+uuid, nil)
	if err != nil {
		return fmt.Errorf("delete VLAN %s: %w", uuid, err)
	}
	c.logger.Info("deleted VLAN", "uuid", uuid)
	return nil
}

// ReconfigureVLANs applies pending VLAN changes.
func (c *Client) ReconfigureVLANs(ctx context.Context) error {
	_, err := c.doRequest(ctx, "POST", "/interfaces/vlan_settings/reconfigure", map[string]any{})
	return err
}

// GetVLANByTag finds an existing VLAN by its tag number (for idempotency).
func (c *Client) GetVLANByTag(ctx context.Context, tag int) (*VLAN, error) {
	resp, err := c.doRequest(ctx, "GET", "/interfaces/vlan_settings/searchItem", nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Rows []VLAN `json:"rows"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("parse VLANs: %w", err)
	}

	tagStr := fmt.Sprintf("%d", tag)
	for _, v := range result.Rows {
		if v.Tag == tagStr {
			return &v, nil
		}
	}
	return nil, nil // not found
}

// ---------- DHCP Operations ----------

// CreateDHCPSubnet creates a Kea DHCPv4 subnet with DNS configured.
// Uses autocollect to set the router from the interface, then patches
// DNS via setSubnet (addSubnet ignores nested option_data fields).
func (c *Client) CreateDHCPSubnet(ctx context.Context, subnet, poolRange, gateway string) (string, error) {
	// Step 1: Create subnet with autocollect (reliably sets router option)
	payload := map[string]any{
		"subnet4": map[string]any{
			"subnet":                  subnet,
			"pools":                   poolRange,
			"option_data_autocollect": "1",
		},
	}

	resp, err := c.doRequest(ctx, "POST", "/kea/dhcpv4/addSubnet", payload)
	if err != nil {
		return "", fmt.Errorf("create DHCP subnet %s: %w", subnet, err)
	}

	var result struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("parse DHCP response: %w", err)
	}

	// Step 2: Patch subnet to add DNS server (setSubnet accepts nested option_data)
	if result.UUID != "" {
		dnsPayload := map[string]any{
			"subnet4": map[string]any{
				"option_data": map[string]any{
					"domain_name_servers": gateway,
				},
			},
		}
		if _, err := c.doRequest(ctx, "POST", "/kea/dhcpv4/setSubnet/"+result.UUID, dnsPayload); err != nil {
			c.logger.Warn("failed to set DNS on DHCP subnet", "uuid", result.UUID, "error", err)
		}
	}

	c.logger.Info("created DHCP subnet", "subnet", subnet, "uuid", result.UUID)
	return result.UUID, nil
}

// DeleteDHCPSubnet removes a DHCP subnet by UUID.
func (c *Client) DeleteDHCPSubnet(ctx context.Context, uuid string) error {
	_, err := c.doRequest(ctx, "POST", "/kea/dhcpv4/delSubnet/"+uuid, nil)
	if err != nil {
		return fmt.Errorf("delete DHCP subnet %s: %w", uuid, err)
	}
	c.logger.Info("deleted DHCP subnet", "uuid", uuid)
	return nil
}

// ReconfigureDHCP applies pending DHCP changes.
func (c *Client) ReconfigureDHCP(ctx context.Context) error {
	_, err := c.doRequest(ctx, "POST", "/kea/service/reconfigure", map[string]any{})
	return err
}

// RestartDHCP fully restarts the Kea DHCP service.
// Required when new interfaces are created, because Kea with raw sockets
// only binds to interfaces that exist at startup.
func (c *Client) RestartDHCP(ctx context.Context) error {
	_, err := c.doRequest(ctx, "POST", "/kea/service/restart", map[string]any{})
	return err
}

// GetDHCPInterfaces returns the currently selected OPNsense interfaces in the
// Kea DHCPv4 configuration.
func (c *Client) GetDHCPInterfaces(ctx context.Context) ([]string, error) {
	resp, err := c.doRequest(ctx, "GET", "/kea/dhcpv4/get", nil)
	if err != nil {
		return nil, fmt.Errorf("get DHCP settings: %w", err)
	}

	var settings struct {
		DHCPV4 struct {
			General struct {
				Interfaces map[string]struct {
					Value    string `json:"value"`
					Selected int    `json:"selected"`
				} `json:"interfaces"`
			} `json:"general"`
		} `json:"dhcpv4"`
	}
	if err := json.Unmarshal(resp, &settings); err != nil {
		return nil, fmt.Errorf("parse DHCP settings: %w", err)
	}

	var selected []string
	for key, iface := range settings.DHCPV4.General.Interfaces {
		if iface.Selected == 1 {
			selected = append(selected, key)
		}
	}
	return selected, nil
}

// AddDHCPInterface adds an OPNsense interface to Kea's listened interfaces list.
// Kea only serves DHCP on explicitly configured interfaces.
func (c *Client) AddDHCPInterface(ctx context.Context, ifName string) error {
	selected, err := c.GetDHCPInterfaces(ctx)
	if err != nil {
		return err
	}

	// Build comma-separated list of selected interfaces + the new one.
	// Add the new interface if not already selected
	found := false
	for _, s := range selected {
		if s == ifName {
			found = true
			break
		}
	}
	if !found {
		selected = append(selected, ifName)
	}

	interfaces := ""
	for i, s := range selected {
		if i > 0 {
			interfaces += ","
		}
		interfaces += s
	}

	payload := map[string]any{
		"dhcpv4": map[string]any{
			"general": map[string]any{
				"interfaces": interfaces,
			},
		},
	}
	_, err = c.doRequest(ctx, "POST", "/kea/dhcpv4/set", payload)
	if err != nil {
		return fmt.Errorf("set DHCP interfaces: %w", err)
	}

	c.logger.Info("added DHCP interface", "interface", ifName, "all_interfaces", interfaces)
	return nil
}

// RemoveDHCPInterface removes an OPNsense interface from Kea's listened interfaces list.
func (c *Client) RemoveDHCPInterface(ctx context.Context, ifName string) error {
	resp, err := c.doRequest(ctx, "GET", "/kea/dhcpv4/get", nil)
	if err != nil {
		return fmt.Errorf("get DHCP settings: %w", err)
	}

	var settings struct {
		DHCPV4 struct {
			General struct {
				Interfaces map[string]struct {
					Value    string `json:"value"`
					Selected int    `json:"selected"`
				} `json:"interfaces"`
			} `json:"general"`
		} `json:"dhcpv4"`
	}
	if err := json.Unmarshal(resp, &settings); err != nil {
		return fmt.Errorf("parse DHCP settings: %w", err)
	}

	var selected []string
	for key, iface := range settings.DHCPV4.General.Interfaces {
		if iface.Selected == 1 && key != ifName {
			selected = append(selected, key)
		}
	}

	interfaces := ""
	for i, s := range selected {
		if i > 0 {
			interfaces += ","
		}
		interfaces += s
	}

	payload := map[string]any{
		"dhcpv4": map[string]any{
			"general": map[string]any{
				"interfaces": interfaces,
			},
		},
	}
	_, err = c.doRequest(ctx, "POST", "/kea/dhcpv4/set", payload)
	if err != nil {
		return fmt.Errorf("set DHCP interfaces: %w", err)
	}

	c.logger.Info("removed DHCP interface", "interface", ifName, "remaining_interfaces", interfaces)
	return nil
}

// GetDHCPSubnetByNetwork finds a DHCP subnet by its network (for idempotency).
func (c *Client) GetDHCPSubnetByNetwork(ctx context.Context, subnet string) (*DHCPSubnet, error) {
	resp, err := c.doRequest(ctx, "GET", "/kea/dhcpv4/searchSubnet", nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Rows []DHCPSubnet `json:"rows"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("parse DHCP subnets: %w", err)
	}

	for _, s := range result.Rows {
		if s.Subnet == subnet {
			return &s, nil
		}
	}
	return nil, nil
}

// ---------- Firewall Operations ----------

// FirewallRule represents a firewall filter rule.
type FirewallRule struct {
	Enabled     string `json:"enabled"`
	Action      string `json:"action"`     // "pass" or "block"
	Interface   string `json:"interface"`  // e.g., "opt2"
	Direction   string `json:"direction"`  // "in"
	IPProtocol  string `json:"ipprotocol"` // "inet"
	Protocol    string `json:"protocol"`   // "any", "TCP", etc.
	Source      string `json:"source_net"` // e.g., "10.100.0.0/24"
	Destination string `json:"destination_net"`
	Description string `json:"descr"`
}

// CreateFirewallRule creates a firewall filter rule.
func (c *Client) CreateFirewallRule(ctx context.Context, rule FirewallRule) (string, error) {
	payload := map[string]any{"rule": rule}

	resp, err := c.doRequest(ctx, "POST", "/firewall/filter/addRule", payload)
	if err != nil {
		return "", fmt.Errorf("create firewall rule: %w", err)
	}

	var result struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("parse firewall response: %w", err)
	}
	return result.UUID, nil
}

// DeleteFirewallRule removes a firewall rule by UUID.
func (c *Client) DeleteFirewallRule(ctx context.Context, uuid string) error {
	_, err := c.doRequest(ctx, "POST", "/firewall/filter/delRule/"+uuid, nil)
	return err
}

// FirewallRuleInfo is a normalized content view of a firewall filter rule, used
// for idempotency checks (e.g. "does a content-equivalent pass rule already
// exist for this pod interface + subnet?").
//
// IMPORTANT: it is sourced from firewall/filter/get, NOT firewall/filter/searchRule.
// searchRule only ever returns a single row (total:1) regardless of how many
// rules exist — it never lists the per-VLAN pod pass rules — so deduping against
// it made the reconciler blind and it re-added an identical rule every cycle
// (the 2026-08-02 config.xml bloat / OPNsense OOM incident). filter/get is the
// authoritative complete ruleset. Every field here is canonicalized (lower-cased,
// trimmed; interface set sorted) so a rule's content signature compares reliably.
type FirewallRuleInfo struct {
	UUID            string
	Interface       string // canonical, sorted, comma-joined selected interface(s), e.g. "opt3" or "lan,opt1"
	Direction       string // "in" / "out" / "any"
	IPProtocol      string // "inet" / "inet6"
	Protocol        string // "any", "tcp", ...
	Source          string // source_net, e.g. "10.100.3.0/24", "any", or an alias like "lan"
	SourcePort      string
	Destination     string // destination_net
	DestinationPort string
	Action          string // "pass" or "block"
}

// opnOption is a single entry in an OPNsense select-field option map, as returned
// by firewall/filter/get: {"<key>": {"value":"<label>","selected":0|1}}.
type opnOption struct {
	Value    string          `json:"value"`
	Selected json.RawMessage `json:"selected"`
}

// isSelected reports whether this option is the/an active selection. OPNsense
// encodes "selected" as either a JSON number (1) or a quoted string ("1")
// depending on version, so both are accepted.
func (o opnOption) isSelected() bool {
	s := strings.Trim(strings.TrimSpace(string(o.Selected)), `"`)
	return s == "1" || strings.EqualFold(s, "true")
}

// filterGetRule mirrors one rule under filter.rules.rule in a firewall/filter/get
// response. Select fields are option-maps; the rest are plain strings.
type filterGetRule struct {
	Interface       map[string]opnOption `json:"interface"`
	Direction       map[string]opnOption `json:"direction"`
	Action          map[string]opnOption `json:"action"`
	IPProtocol      map[string]opnOption `json:"ipprotocol"`
	Protocol        map[string]opnOption `json:"protocol"`
	SourceNet       string               `json:"source_net"`
	SourcePort      string               `json:"source_port"`
	DestinationNet  string               `json:"destination_net"`
	DestinationPort string               `json:"destination_port"`
}

// GetFirewallRules returns the COMPLETE set of firewall filter rules via
// firewall/filter/get (used for reconciler idempotency). See FirewallRuleInfo
// for why filter/get is used instead of searchRule.
func (c *Client) GetFirewallRules(ctx context.Context) ([]FirewallRuleInfo, error) {
	resp, err := c.doRequest(ctx, "GET", "/firewall/filter/get", nil)
	if err != nil {
		return nil, fmt.Errorf("get firewall rules: %w", err)
	}
	var result struct {
		Filter struct {
			Rules struct {
				Rule map[string]filterGetRule `json:"rule"`
			} `json:"rules"`
		} `json:"filter"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("parse firewall rules: %w", err)
	}
	out := make([]FirewallRuleInfo, 0, len(result.Filter.Rules.Rule))
	for uuid, r := range result.Filter.Rules.Rule {
		out = append(out, FirewallRuleInfo{
			UUID:            strings.TrimSpace(uuid),
			Interface:       selectedOptionSet(r.Interface),
			Direction:       selectedOption(r.Direction),
			IPProtocol:      selectedOption(r.IPProtocol),
			Protocol:        selectedOption(r.Protocol),
			Source:          canonicalFirewallField(r.SourceNet),
			SourcePort:      canonicalFirewallField(r.SourcePort),
			Destination:     canonicalFirewallField(r.DestinationNet),
			DestinationPort: canonicalFirewallField(r.DestinationPort),
			Action:          selectedOption(r.Action),
		})
	}
	return out, nil
}

// selectedOptionSet returns the canonical, sorted, comma-joined set of selected
// keys from an OPNsense multi-select option map (e.g. the interface field).
func selectedOptionSet(m map[string]opnOption) string {
	out := make([]string, 0, len(m))
	for k, opt := range m {
		if opt.isSelected() {
			if k = canonicalFirewallField(k); k != "" {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// selectedOption returns the single selected key from an OPNsense select option
// map (canonicalized). If several are somehow selected the result is still the
// canonical sorted set, which keeps comparisons stable.
func selectedOption(m map[string]opnOption) string {
	return selectedOptionSet(m)
}

// canonicalFirewallField normalizes a firewall enum/string field to a stable
// comparison form.
func canonicalFirewallField(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// ApplyFirewall applies pending firewall changes.
func (c *Client) ApplyFirewall(ctx context.Context) error {
	_, err := c.doRequest(ctx, "POST", "/firewall/filter/apply", map[string]any{})
	return err
}
