package opnsense

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// DNSBLPolicy is the source-scoped Unbound blocklist policy shape exposed by
// OPNsense 26.1 under unbound/settings/*Dnsbl.
type DNSBLPolicy struct {
	UUID        string `json:"-"`
	Enabled     string `json:"enabled"`
	Types       string `json:"type"`
	Lists       string `json:"lists"`
	Allowlists  string `json:"allowlists"`
	Blocklists  string `json:"blocklists"`
	Wildcards   string `json:"wildcards"`
	SourceNets  string `json:"source_nets"`
	Address     string `json:"address"`
	NXDomain    string `json:"nxdomain"`
	CacheTTL    string `json:"cache_ttl"`
	Description string `json:"description"`
}

// SupportsSourceScopedSafeSearch is deliberately false for this client.
// OPNsense's built-in Force SafeSearch switch is global. Source-scoped rewrites
// are possible through custom Unbound views, but this client does not yet own,
// validate, activate, or roll back those unmanaged configuration fragments.
func (*Client) SupportsSourceScopedSafeSearch(context.Context) (bool, error) {
	return false, nil
}

// VerifySourceScopedContentFilter cannot be implemented through the OPNsense
// model API alone. DNSBL activation is asynchronous and its action masks shell
// failures, so an effective check must flush/query uncached controlled names
// from a student-source network.
func (*Client) VerifySourceScopedContentFilter(context.Context, string) error {
	return fmt.Errorf("effective student-source DNS verification is not implemented")
}

// GetUnboundSafeSearch reports the global Unbound SafeSearch setting.
func (c *Client) GetUnboundSafeSearch(ctx context.Context) (bool, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, "/unbound/settings/get", nil)
	if err != nil {
		return false, fmt.Errorf("get Unbound settings: %w", err)
	}
	var result struct {
		Unbound struct {
			General struct {
				SafeSearch json.RawMessage `json:"safesearch"`
			} `json:"general"`
		} `json:"unbound"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return false, fmt.Errorf("parse Unbound settings: %w", err)
	}
	value, err := scalarModelValue(result.Unbound.General.SafeSearch)
	if err != nil {
		return false, fmt.Errorf("parse Unbound safesearch: %w", err)
	}
	switch strings.ToLower(value) {
	case "1", "true":
		return true, nil
	case "0", "false", "":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected Unbound safesearch value %q", value)
	}
}

// SetUnboundSafeSearch updates the global Unbound SafeSearch setting. Call
// ReconfigureUnbound after batching Unbound general-setting changes.
func (c *Client) SetUnboundSafeSearch(ctx context.Context, enabled bool) error {
	value := "0"
	if enabled {
		value = "1"
	}
	resp, err := c.doRequest(ctx, http.MethodPost, "/unbound/settings/set", map[string]any{
		"unbound": map[string]any{
			"general": map[string]any{"safesearch": value},
		},
	})
	if err != nil {
		return fmt.Errorf("set Unbound safesearch: %w", err)
	}
	return validateModelMutation(resp, false, "saved")
}

// ListDNSBLPolicies returns the configured DNSBL policy rows. The search
// endpoint is authoritative for DNSBL rows; unlike firewall/searchRule, this
// controller uses the normal MVC searchBase implementation.
func (c *Client) ListDNSBLPolicies(ctx context.Context) ([]DNSBLPolicy, error) {
	const maxDNSBLPolicies = 1000
	resp, err := c.doRequest(ctx, http.MethodPost, "/unbound/settings/searchDnsbl", map[string]any{
		"current":  1,
		"rowCount": maxDNSBLPolicies,
	})
	if err != nil {
		return nil, fmt.Errorf("list Unbound DNSBL policies: %w", err)
	}
	var result struct {
		Rows []struct {
			UUID        string `json:"uuid"`
			Enabled     string `json:"enabled"`
			Description string `json:"description"`
		} `json:"rows"`
		Total json.RawMessage `json:"total"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("parse Unbound DNSBL policies: %w", err)
	}
	totalValue, err := scalarModelValue(result.Total)
	if err != nil {
		return nil, fmt.Errorf("parse Unbound DNSBL policy total: %w", err)
	}
	total, err := strconv.Atoi(totalValue)
	if err != nil || total < 0 {
		return nil, fmt.Errorf("invalid Unbound DNSBL policy total %q", totalValue)
	}
	if total > maxDNSBLPolicies || len(result.Rows) != total {
		return nil, fmt.Errorf("incomplete Unbound DNSBL policy inventory: got %d rows, total %d, limit %d",
			len(result.Rows), total, maxDNSBLPolicies)
	}
	out := make([]DNSBLPolicy, 0, len(result.Rows))
	for _, row := range result.Rows {
		out = append(out, DNSBLPolicy{
			UUID:        strings.TrimSpace(row.UUID),
			Enabled:     strings.TrimSpace(row.Enabled),
			Description: strings.TrimSpace(row.Description),
		})
	}
	return out, nil
}

// GetDNSBLPolicy returns one policy with OptionField values normalized to a
// canonical comma-separated selection.
func (c *Client) GetDNSBLPolicy(ctx context.Context, uuid string) (DNSBLPolicy, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, "/unbound/settings/getDnsbl/"+uuid, nil)
	if err != nil {
		return DNSBLPolicy{}, fmt.Errorf("get Unbound DNSBL policy %s: %w", uuid, err)
	}
	var result struct {
		Blocklist struct {
			Enabled     json.RawMessage      `json:"enabled"`
			Types       map[string]opnOption `json:"type"`
			Lists       json.RawMessage      `json:"lists"`
			Allowlists  json.RawMessage      `json:"allowlists"`
			Blocklists  json.RawMessage      `json:"blocklists"`
			Wildcards   json.RawMessage      `json:"wildcards"`
			SourceNets  json.RawMessage      `json:"source_nets"`
			Address     json.RawMessage      `json:"address"`
			NXDomain    json.RawMessage      `json:"nxdomain"`
			CacheTTL    json.RawMessage      `json:"cache_ttl"`
			Description json.RawMessage      `json:"description"`
		} `json:"blocklist"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return DNSBLPolicy{}, fmt.Errorf("parse Unbound DNSBL policy %s: %w", uuid, err)
	}
	scalar := func(field string, raw json.RawMessage) (string, error) {
		value, err := scalarModelValue(raw)
		if err != nil {
			return "", fmt.Errorf("%s: %w", field, err)
		}
		return strings.TrimSpace(value), nil
	}
	enabled, err := scalar("enabled", result.Blocklist.Enabled)
	if err != nil {
		return DNSBLPolicy{}, err
	}
	lists, err := scalar("lists", result.Blocklist.Lists)
	if err != nil {
		return DNSBLPolicy{}, err
	}
	allowlists, err := scalar("allowlists", result.Blocklist.Allowlists)
	if err != nil {
		return DNSBLPolicy{}, err
	}
	blocklists, err := scalar("blocklists", result.Blocklist.Blocklists)
	if err != nil {
		return DNSBLPolicy{}, err
	}
	wildcards, err := scalar("wildcards", result.Blocklist.Wildcards)
	if err != nil {
		return DNSBLPolicy{}, err
	}
	sourceNets, err := modelListValue(result.Blocklist.SourceNets)
	if err != nil {
		return DNSBLPolicy{}, fmt.Errorf("source_nets: %w", err)
	}
	address, err := scalar("address", result.Blocklist.Address)
	if err != nil {
		return DNSBLPolicy{}, err
	}
	nxdomain, err := scalar("nxdomain", result.Blocklist.NXDomain)
	if err != nil {
		return DNSBLPolicy{}, err
	}
	cacheTTL, err := scalar("cache_ttl", result.Blocklist.CacheTTL)
	if err != nil {
		return DNSBLPolicy{}, err
	}
	description, err := scalar("description", result.Blocklist.Description)
	if err != nil {
		return DNSBLPolicy{}, err
	}
	types, err := selectedOptionSetStrict(result.Blocklist.Types, false)
	if err != nil {
		return DNSBLPolicy{}, fmt.Errorf("type: %w", err)
	}
	return DNSBLPolicy{
		UUID:        strings.TrimSpace(uuid),
		Enabled:     enabled,
		Types:       types,
		Lists:       lists,
		Allowlists:  allowlists,
		Blocklists:  blocklists,
		Wildcards:   wildcards,
		SourceNets:  sourceNets,
		Address:     address,
		NXDomain:    nxdomain,
		CacheTTL:    cacheTTL,
		Description: description,
	}, nil
}

func (c *Client) CreateDNSBLPolicy(ctx context.Context, policy DNSBLPolicy) (string, error) {
	resp, err := c.doRequest(ctx, http.MethodPost, "/unbound/settings/addDnsbl", map[string]any{"blocklist": policy})
	if err != nil {
		return "", fmt.Errorf("create Unbound DNSBL policy: %w", err)
	}
	if err := validateModelMutation(resp, true, "saved"); err != nil {
		return "", err
	}
	var result struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("parse Unbound DNSBL create response: %w", err)
	}
	return strings.TrimSpace(result.UUID), nil
}

func (c *Client) UpdateDNSBLPolicy(ctx context.Context, uuid string, policy DNSBLPolicy) error {
	resp, err := c.doRequest(ctx, http.MethodPost, "/unbound/settings/setDnsbl/"+uuid, map[string]any{"blocklist": policy})
	if err != nil {
		return fmt.Errorf("update Unbound DNSBL policy %s: %w", uuid, err)
	}
	return validateModelMutation(resp, false, "saved")
}

func (c *Client) DeleteDNSBLPolicy(ctx context.Context, uuid string) error {
	resp, err := c.doRequest(ctx, http.MethodPost, "/unbound/settings/delDnsbl/"+uuid, nil)
	if err != nil {
		return fmt.Errorf("delete Unbound DNSBL policy %s: %w", uuid, err)
	}
	return validateModelMutation(resp, false, "deleted")
}

func (c *Client) ReconfigureUnbound(ctx context.Context) error {
	resp, err := c.doRequest(ctx, http.MethodPost, "/unbound/service/reconfigureGeneral", map[string]any{})
	if err != nil {
		return fmt.Errorf("reconfigure Unbound: %w", err)
	}
	if err := validateServiceMutation(resp); err != nil {
		return fmt.Errorf("reconfigure Unbound: %w", err)
	}
	return nil
}

func (c *Client) RefreshUnboundDNSBL(ctx context.Context) error {
	resp, err := c.doRequest(ctx, http.MethodPost, "/unbound/service/dnsbl", map[string]any{})
	if err != nil {
		return fmt.Errorf("refresh Unbound DNSBL: %w", err)
	}
	if err := validateServiceMutation(resp); err != nil {
		return fmt.Errorf("refresh Unbound DNSBL: %w", err)
	}
	return nil
}

func modelListValue(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var options map[string]opnOption
	if err := json.Unmarshal(raw, &options); err == nil && options != nil {
		return selectedOptionSetStrict(options, false)
	}
	return scalarModelValue(raw)
}

func scalarModelValue(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var scalar any
	if err := json.Unmarshal(raw, &scalar); err != nil {
		return "", err
	}
	switch value := scalar.(type) {
	case string:
		return value, nil
	case float64:
		return fmt.Sprintf("%g", value), nil
	case bool:
		return fmt.Sprintf("%t", value), nil
	case map[string]any:
		for key, entry := range value {
			option, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			selected := fmt.Sprint(option["selected"])
			if selected == "1" || strings.EqualFold(selected, "true") {
				return key, nil
			}
		}
		return "", nil
	default:
		return "", fmt.Errorf("unsupported model value type %T", scalar)
	}
}

func validateModelMutation(body []byte, requireUUID bool, acceptedResults ...string) error {
	var result struct {
		UUID        string         `json:"uuid"`
		Result      string         `json:"result"`
		Status      string         `json:"status"`
		Validations map[string]any `json:"validations"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse OPNsense mutation response: %w", err)
	}
	if len(result.Validations) > 0 {
		return fmt.Errorf("OPNsense validation failed: %v", result.Validations)
	}
	if requireUUID && strings.TrimSpace(result.UUID) == "" {
		return fmt.Errorf("OPNsense mutation response omitted uuid")
	}
	accepted := false
	for _, expected := range acceptedResults {
		if strings.EqualFold(result.Result, expected) {
			accepted = true
			break
		}
	}
	if !accepted {
		return fmt.Errorf("OPNsense mutation result %q, want one of %v", result.Result, acceptedResults)
	}
	if result.Status != "" && !strings.EqualFold(result.Status, "ok") {
		return fmt.Errorf("OPNsense mutation status %q", result.Status)
	}
	return nil
}

func validateServiceMutation(body []byte) error {
	var result struct {
		Status      string         `json:"status"`
		Validations map[string]any `json:"validations"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse OPNsense service response: %w", err)
	}
	if len(result.Validations) > 0 {
		return fmt.Errorf("OPNsense service validation failed: %v", result.Validations)
	}
	if !strings.EqualFold(result.Status, "ok") {
		return fmt.Errorf("OPNsense service status %q", result.Status)
	}
	return nil
}
