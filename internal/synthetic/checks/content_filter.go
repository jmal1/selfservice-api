package checks

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jmal1/selfservice-api/internal/opnsense"
	"github.com/jmal1/selfservice-api/internal/provisioner"
	"github.com/jmal1/selfservice-api/internal/synthetic"
)

type ContentFilterReader interface {
	GetFirewallRules(ctx context.Context) ([]opnsense.FirewallRuleInfo, error)
	SupportsSourceScopedSafeSearch(ctx context.Context) (bool, error)
	VerifySourceScopedContentFilter(ctx context.Context, sourceNetwork string) error
	ListDNSBLPolicies(ctx context.Context) ([]opnsense.DNSBLPolicy, error)
	GetDNSBLPolicy(ctx context.Context, uuid string) (opnsense.DNSBLPolicy, error)
}

type ContentFilterPolicyConfig struct {
	Reader            ContentFilterReader
	Policy            provisioner.ContentFilterConfig
	MaxGeneratedRules int
}

// ContentFilterPolicy is a read-only OPNsense policy check. When policy is not
// expected it remains registered and reports success, preserving the stable
// metric name while rollout is deliberately disabled.
func ContentFilterPolicy(cfg ContentFilterPolicyConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:        "content_filter_policy",
		TitleVal:       "Student Content Filter Policy",
		DescriptionVal: "Reads OPNsense without mutating it. Requires client-managed source-scoped SafeSearch, verifies global quick bypass controls and source-scoped DNSBL configuration, then requires effective student-source queries.",
		RunbookVal:     "https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/troubleshooting.md#student-content-filter-policy",
		SeverityVal:    synthetic.SeverityCritical,
		RunFn: func(ctx context.Context, _ *synthetic.Client) (int, error) {
			if !cfg.Policy.Enabled {
				return http.StatusOK, nil
			}
			if cfg.Reader == nil {
				return 0, fmt.Errorf("content filter is expected but no OPNsense read-only client is configured")
			}
			supported, err := cfg.Reader.SupportsSourceScopedSafeSearch(ctx)
			if err != nil {
				return 0, fmt.Errorf("check source-scoped SafeSearch support: %w", err)
			}
			if !supported {
				return 0, fmt.Errorf("content filter is expected but OPNsense 26.1 built-in Force SafeSearch is global and this integration does not manage or verify the proven custom Unbound view")
			}
			rules, err := cfg.Reader.GetFirewallRules(ctx)
			if err != nil {
				return 0, fmt.Errorf("read firewall policy: %w", err)
			}
			if err := provisioner.InspectGeneratedPodFirewallRules(rules, cfg.MaxGeneratedRules); err != nil {
				return http.StatusOK, err
			}
			rows, err := cfg.Reader.ListDNSBLPolicies(ctx)
			if err != nil {
				return 0, fmt.Errorf("list DNSBL policy: %w", err)
			}
			policies := make([]opnsense.DNSBLPolicy, 0, len(rows))
			for _, row := range rows {
				if row.Description != provisioner.ContentFilterDNSBLDescription {
					continue
				}
				policy, err := cfg.Reader.GetDNSBLPolicy(ctx, row.UUID)
				if err != nil {
					return 0, fmt.Errorf("read DNSBL policy %s: %w", row.UUID, err)
				}
				policies = append(policies, policy)
			}
			if err := provisioner.InspectContentFilterPolicy(rules, policies, cfg.Policy); err != nil {
				return http.StatusOK, err
			}
			if err := cfg.Reader.VerifySourceScopedContentFilter(ctx, cfg.Policy.SourceNetwork); err != nil {
				return http.StatusOK, fmt.Errorf("verify effective student-source policy: %w", err)
			}
			return http.StatusOK, nil
		},
	}
}
