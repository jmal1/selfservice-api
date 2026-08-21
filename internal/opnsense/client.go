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
	Enabled           string `json:"enabled"`
	Sequence          string `json:"sequence,omitempty"`
	Quick             string `json:"quick,omitempty"`
	Action            string `json:"action"`    // "pass" or "block"
	Interface         string `json:"interface"` // empty means global/floating
	InterfaceInvert   string `json:"interfacenot,omitempty"`
	Direction         string `json:"direction"`  // "in"
	IPProtocol        string `json:"ipprotocol"` // "inet"
	Protocol          string `json:"protocol"`   // "any", "TCP", etc.
	SourceInvert      string `json:"source_not,omitempty"`
	Source            string `json:"source_net"` // e.g., "10.100.0.0/24"
	SourcePort        string `json:"source_port,omitempty"`
	DestinationInvert string `json:"destination_not,omitempty"`
	Destination       string `json:"destination_net"`
	DestinationPort   string `json:"destination_port,omitempty"`
	Log               string `json:"log,omitempty"`
	Description       string `json:"description"`
}

// CreateFirewallRule creates a firewall filter rule.
func (c *Client) CreateFirewallRule(ctx context.Context, rule FirewallRule) (string, error) {
	payload := map[string]any{"rule": rule}

	resp, err := c.doRequest(ctx, "POST", "/firewall/filter/addRule", payload)
	if err != nil {
		return "", fmt.Errorf("create firewall rule: %w", err)
	}

	if err := validateModelMutation(resp, true, "saved"); err != nil {
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

// UpdateFirewallRule replaces a firewall filter rule by UUID.
func (c *Client) UpdateFirewallRule(ctx context.Context, uuid string, rule FirewallRule) error {
	if strings.TrimSpace(uuid) == "" {
		return fmt.Errorf("update firewall rule: uuid is required")
	}
	resp, err := c.doRequest(ctx, "POST", "/firewall/filter/setRule/"+uuid, map[string]any{"rule": rule})
	if err != nil {
		return fmt.Errorf("update firewall rule %s: %w", uuid, err)
	}
	if err := validateModelMutation(resp, false, "saved"); err != nil {
		return fmt.Errorf("update firewall rule %s: %w", uuid, err)
	}
	return nil
}

// DeleteFirewallRule removes a firewall rule by UUID.
func (c *Client) DeleteFirewallRule(ctx context.Context, uuid string) error {
	resp, err := c.doRequest(ctx, "POST", "/firewall/filter/delRule/"+uuid, nil)
	if err != nil {
		return fmt.Errorf("delete firewall rule %s: %w", uuid, err)
	}
	if err := validateModelMutation(resp, false, "deleted"); err != nil {
		return fmt.Errorf("delete firewall rule %s: %w", uuid, err)
	}
	return nil
}

// FirewallRuleInfo is a normalized content view of a firewall filter rule, used
// for idempotency checks (e.g. "does a content-equivalent pass rule already
// exist for this pod interface + subnet?").
//
// IMPORTANT: it is sourced from firewall/filter/get, NOT firewall/filter/searchRule.
// searchRule only ever returns a single row (total:1) regardless of how many
// rules exist — it never lists the per-VLAN pod pass rules — so deduping against
// it made the reconciler blind and it re-added an identical rule every cycle
// (the 2026-08-21 config.xml bloat incident). filter/get is the
// authoritative complete ruleset. Every field here is canonicalized (lower-cased,
// trimmed; interface set sorted) so a rule's content signature compares reliably.
type FirewallRuleInfo struct {
	UUID              string
	Enabled           string
	Sequence          string
	Quick             string
	Interface         string // canonical, sorted, comma-joined selected interface(s), e.g. "opt3" or "lan,opt1"
	InterfaceInvert   string
	Direction         string // "in" / "out" / "any"
	IPProtocol        string // "inet" / "inet6"
	Protocol          string // "any", "tcp", ...
	SourceInvert      string
	Source            string // source_net, e.g. "10.100.3.0/24", "any", or an alias like "lan"
	SourcePort        string
	DestinationInvert string
	Destination       string // destination_net
	DestinationPort   string
	Action            string // "pass" or "block"
	Log               string
	Description       string
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
	Enabled           string               `json:"enabled"`
	Sequence          string               `json:"sequence"`
	Quick             string               `json:"quick"`
	Interface         map[string]opnOption `json:"interface"`
	InterfaceInvert   string               `json:"interfacenot"`
	Direction         map[string]opnOption `json:"direction"`
	Action            map[string]opnOption `json:"action"`
	IPProtocol        map[string]opnOption `json:"ipprotocol"`
	Protocol          map[string]opnOption `json:"protocol"`
	SourceInvert      string               `json:"source_not"`
	SourceNet         string               `json:"source_net"`
	SourcePort        string               `json:"source_port"`
	DestinationInvert string               `json:"destination_not"`
	DestinationNet    string               `json:"destination_net"`
	DestinationPort   string               `json:"destination_port"`
	Log               string               `json:"log"`
	Description       string               `json:"description"`
}

const maxFirewallRulesResponseBytes = 128 << 20

// GetFirewallRules returns the COMPLETE set of firewall filter rules via
// firewall/filter/get (used for reconciler idempotency). See FirewallRuleInfo
// for why filter/get is used instead of searchRule.
//
// Live-verified fact (2026-08-21, fwpodv01): searchRule returns total:1
// regardless of the actual rule count; firewall/filter/get is authoritative and
// returns every rule. Do not switch this back to searchRule.
func (c *Client) GetFirewallRules(ctx context.Context) ([]FirewallRuleInfo, error) {
	var out []FirewallRuleInfo
	err := c.doStreamingJSONRequest(ctx, http.MethodGet, "/firewall/filter/get", maxFirewallRulesResponseBytes, func(r io.Reader) error {
		var err error
		out, err = decodeFirewallRules(r)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("get firewall rules: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
	return out, nil
}

// doStreamingJSONRequest executes a bounded GET and decodes the body directly
// from the socket. filter/get can be tens of megabytes because every rule
// repeats every interface option; buffering the response and then unmarshalling
// it temporarily doubles that cost in the worker.
func (c *Client) doStreamingJSONRequest(
	ctx context.Context,
	method, path string,
	maxBytes int64,
	decode func(io.Reader) error,
) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			c.logger.Info("retrying OPNsense streaming API call", "path", path, "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, c.config.BaseURL+path, nil)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.SetBasicAuth(c.config.APIKey, c.config.APISecret)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			continue
		}
		if resp.StatusCode >= http.StatusBadRequest {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
			if resp.StatusCode >= http.StatusInternalServerError {
				continue
			}
			return lastErr
		}

		limited := &io.LimitedReader{R: resp.Body, N: maxBytes + 1}
		err = decode(limited)
		resp.Body.Close()
		if limited.N <= 0 {
			return fmt.Errorf("response exceeds %d bytes", maxBytes)
		}
		if err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return nil
	}
	return fmt.Errorf("all retries exhausted: %w", lastErr)
}

func decodeFirewallRules(r io.Reader) ([]FirewallRuleInfo, error) {
	dec := json.NewDecoder(r)
	if err := expectJSONDelim(dec, '{'); err != nil {
		return nil, err
	}
	var out []FirewallRuleInfo
	foundFilter := false
	for dec.More() {
		key, err := nextJSONKey(dec)
		if err != nil {
			return nil, err
		}
		if key != "filter" {
			if err := discardJSONValue(dec); err != nil {
				return nil, err
			}
			continue
		}
		foundFilter = true
		out, err = decodeFirewallFilterObject(dec)
		if err != nil {
			return nil, err
		}
	}
	if err := expectJSONDelim(dec, '}'); err != nil {
		return nil, err
	}
	if !foundFilter {
		return nil, fmt.Errorf("missing filter object")
	}
	if tok, err := dec.Token(); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("read trailing JSON: %w", err)
		}
		return nil, fmt.Errorf("unexpected trailing JSON token %v", tok)
	}
	return out, nil
}

func decodeFirewallFilterObject(dec *json.Decoder) ([]FirewallRuleInfo, error) {
	if err := expectJSONDelim(dec, '{'); err != nil {
		return nil, fmt.Errorf("filter: %w", err)
	}
	var out []FirewallRuleInfo
	foundRules := false
	for dec.More() {
		key, err := nextJSONKey(dec)
		if err != nil {
			return nil, err
		}
		if key != "rules" {
			if err := discardJSONValue(dec); err != nil {
				return nil, err
			}
			continue
		}
		foundRules = true
		out, err = decodeFirewallRulesObject(dec)
		if err != nil {
			return nil, err
		}
	}
	if err := expectJSONDelim(dec, '}'); err != nil {
		return nil, err
	}
	if !foundRules {
		return nil, fmt.Errorf("filter missing rules object")
	}
	return out, nil
}

func decodeFirewallRulesObject(dec *json.Decoder) ([]FirewallRuleInfo, error) {
	if err := expectJSONDelim(dec, '{'); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	var out []FirewallRuleInfo
	foundRuleMap := false
	for dec.More() {
		key, err := nextJSONKey(dec)
		if err != nil {
			return nil, err
		}
		if key != "rule" {
			if err := discardJSONValue(dec); err != nil {
				return nil, err
			}
			continue
		}
		foundRuleMap = true
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("rule map: %w", err)
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil, fmt.Errorf("rule map: expected object or empty array, got %v", tok)
		}
		if delim == '[' {
			if dec.More() {
				return nil, fmt.Errorf("rule map: non-empty array is invalid")
			}
			if err := expectJSONDelim(dec, ']'); err != nil {
				return nil, fmt.Errorf("rule map: %w", err)
			}
			continue
		}
		if delim != '{' {
			return nil, fmt.Errorf("rule map: expected object or empty array, got %q", delim)
		}
		for dec.More() {
			uuid, err := nextJSONKey(dec)
			if err != nil {
				return nil, err
			}
			var rule filterGetRule
			if err := dec.Decode(&rule); err != nil {
				return nil, fmt.Errorf("rule %q: %w", uuid, err)
			}
			normalized, err := normalizeFilterGetRule(uuid, rule)
			if err != nil {
				return nil, fmt.Errorf("rule %q: %w", uuid, err)
			}
			out = append(out, normalized)
		}
		if err := expectJSONDelim(dec, '}'); err != nil {
			return nil, err
		}
	}
	if err := expectJSONDelim(dec, '}'); err != nil {
		return nil, err
	}
	if !foundRuleMap {
		return nil, fmt.Errorf("rules missing rule map")
	}
	return out, nil
}

func normalizeFilterGetRule(uuid string, r filterGetRule) (FirewallRuleInfo, error) {
	if r.Interface == nil {
		return FirewallRuleInfo{}, fmt.Errorf("missing interface option map")
	}
	interfaces, err := selectedOptionSetStrict(r.Interface, false)
	if err != nil {
		return FirewallRuleInfo{}, fmt.Errorf("interface: %w", err)
	}
	direction, err := selectedOptionSetStrict(r.Direction, true)
	if err != nil {
		return FirewallRuleInfo{}, fmt.Errorf("direction: %w", err)
	}
	ipProtocol, err := selectedOptionSetStrict(r.IPProtocol, true)
	if err != nil {
		return FirewallRuleInfo{}, fmt.Errorf("ipprotocol: %w", err)
	}
	protocol, err := selectedOptionSetStrict(r.Protocol, true)
	if err != nil {
		return FirewallRuleInfo{}, fmt.Errorf("protocol: %w", err)
	}
	action, err := selectedOptionSetStrict(r.Action, true)
	if err != nil {
		return FirewallRuleInfo{}, fmt.Errorf("action: %w", err)
	}
	return FirewallRuleInfo{
		UUID:              strings.TrimSpace(uuid),
		Enabled:           canonicalFirewallField(r.Enabled),
		Sequence:          canonicalFirewallField(r.Sequence),
		Quick:             canonicalFirewallField(r.Quick),
		Interface:         interfaces,
		InterfaceInvert:   canonicalFirewallField(r.InterfaceInvert),
		Direction:         direction,
		IPProtocol:        ipProtocol,
		Protocol:          protocol,
		SourceInvert:      canonicalFirewallField(r.SourceInvert),
		Source:            canonicalFirewallField(r.SourceNet),
		SourcePort:        canonicalFirewallField(r.SourcePort),
		DestinationInvert: canonicalFirewallField(r.DestinationInvert),
		Destination:       canonicalFirewallField(r.DestinationNet),
		DestinationPort:   canonicalFirewallField(r.DestinationPort),
		Action:            action,
		Log:               canonicalFirewallField(r.Log),
		Description:       strings.TrimSpace(r.Description),
	}, nil
}

func expectJSONDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	got, ok := tok.(json.Delim)
	if !ok || got != want {
		return fmt.Errorf("expected %q, got %v", want, tok)
	}
	return nil
}

func nextJSONKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	key, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("expected object key, got %v", tok)
	}
	return key, nil
}

func discardJSONValue(dec *json.Decoder) error {
	var discard json.RawMessage
	return dec.Decode(&discard)
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

func selectedOptionSetStrict(m map[string]opnOption, requireSingle bool) (string, error) {
	if m == nil {
		return "", fmt.Errorf("missing option map")
	}
	out := make([]string, 0, len(m))
	for key, option := range m {
		raw := strings.ToLower(strings.Trim(strings.TrimSpace(string(option.Selected)), `"`))
		switch raw {
		case "1", "true":
			if key = canonicalFirewallField(key); key != "" {
				out = append(out, key)
			}
		case "0", "false":
		default:
			return "", fmt.Errorf("option %q has unsupported selected value %q", key, raw)
		}
	}
	sort.Strings(out)
	if requireSingle && len(out) != 1 {
		return "", fmt.Errorf("has %d selected values, want exactly 1", len(out))
	}
	return strings.Join(out, ","), nil
}

// canonicalFirewallField normalizes a firewall enum/string field to a stable
// comparison form.
func canonicalFirewallField(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// ApplyFirewall applies pending firewall changes.
func (c *Client) ApplyFirewall(ctx context.Context) error {
	resp, err := c.doRequest(ctx, "POST", "/firewall/filter/apply", map[string]any{})
	if err != nil {
		return err
	}
	if err := validateServiceMutation(resp); err != nil {
		return fmt.Errorf("apply firewall: %w", err)
	}
	return nil
}
