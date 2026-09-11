package checks

import (
	"context"
	"errors"
	"testing"

	"github.com/jmal1/selfservice-api/internal/opnsense"
	"github.com/jmal1/selfservice-api/internal/provisioner"
	"github.com/jmal1/selfservice-api/internal/synthetic"
)

type fakeContentFilterReader struct {
	rules        []opnsense.FirewallRuleInfo
	rulesErr     error
	ruleCalls    int
	scopedSearch bool
}

func (f *fakeContentFilterReader) GetFirewallRules(context.Context) ([]opnsense.FirewallRuleInfo, error) {
	f.ruleCalls++
	return f.rules, f.rulesErr
}

func (f *fakeContentFilterReader) SupportsSourceScopedSafeSearch(context.Context) (bool, error) {
	return f.scopedSearch, nil
}

func (*fakeContentFilterReader) VerifySourceScopedContentFilter(context.Context, string) error {
	return errors.New("unexpected runtime verification")
}

func (*fakeContentFilterReader) ListDNSBLPolicies(context.Context) ([]opnsense.DNSBLPolicy, error) {
	return nil, errors.New("unexpected DNSBL read")
}

func (*fakeContentFilterReader) GetDNSBLPolicy(context.Context, string) (opnsense.DNSBLPolicy, error) {
	return opnsense.DNSBLPolicy{}, errors.New("unexpected DNSBL policy read")
}

func TestContentFilterPolicy_RemainsGreenWhileActivationIsDeliberatelyDisabled(t *testing.T) {
	check := ContentFilterPolicy(ContentFilterPolicyConfig{
		Policy: provisioner.ContentFilterConfig{Enabled: false},
	})
	status, err := check.Run(context.Background(), synthetic.NewClient("http://unused", ""))
	if err != nil || status != 200 {
		t.Fatalf("disabled policy check: status=%d err=%v", status, err)
	}
}

func TestContentFilterPolicy_ExpectedWithoutReaderFailsLoudly(t *testing.T) {
	check := ContentFilterPolicy(ContentFilterPolicyConfig{
		Policy: provisioner.ContentFilterConfig{
			Enabled:             true,
			SourceNetwork:       "10.100.0.0/16",
			CategoryFeedBaseURL: "https://student-filter-feed.example.test",
		},
	})
	if _, err := check.Run(context.Background(), synthetic.NewClient("http://unused", "")); err == nil {
		t.Fatal("expected policy without OPNsense credentials/reader must fail, not disappear")
	}
}

func TestContentFilterPolicy_UsesDedicatedReaderAndEnforcesGeneratedRuleCeiling(t *testing.T) {
	rules := make([]opnsense.FirewallRuleInfo, 0, 2)
	for i := 0; i < 2; i++ {
		rules = append(rules, opnsense.FirewallRuleInfo{
			UUID:        string(rune('a' + i)),
			Enabled:     "1",
			Quick:       "1",
			Action:      "pass",
			Interface:   "opt6",
			Direction:   "in",
			IPProtocol:  "inet",
			Protocol:    "any",
			Source:      "10.100.15.0/24",
			Destination: "any",
		})
	}
	reader := &fakeContentFilterReader{rules: rules, scopedSearch: true}
	check := ContentFilterPolicy(ContentFilterPolicyConfig{
		Reader: reader,
		Policy: provisioner.ContentFilterConfig{
			Enabled:             true,
			SourceNetwork:       "10.100.0.0/16",
			CategoryFeedBaseURL: "https://student-filter-feed.example.test",
		},
		MaxGeneratedRules: 1,
	})
	if _, err := check.Run(context.Background(), synthetic.NewClient("http://unused", "")); err == nil {
		t.Fatal("generated-rule ceiling must fail before unrelated policy reads")
	}
	if reader.ruleCalls != 1 {
		t.Fatalf("dedicated OPNsense reader calls = %d, want 1", reader.ruleCalls)
	}
}

func TestContentFilterPolicy_UnmanagedSourceScopedSafeSearchFailsBeforeInventoryRead(t *testing.T) {
	reader := &fakeContentFilterReader{}
	check := ContentFilterPolicy(ContentFilterPolicyConfig{
		Reader: reader,
		Policy: provisioner.ContentFilterConfig{
			Enabled:             true,
			SourceNetwork:       "10.100.0.0/16",
			CategoryFeedBaseURL: "https://student-filter-feed.example.test",
		},
	})
	if _, err := check.Run(context.Background(), synthetic.NewClient("http://unused", "")); err == nil {
		t.Fatal("unmanaged source-scoped SafeSearch must keep student-only activation red")
	}
	if reader.ruleCalls != 0 {
		t.Fatalf("unsupported activation read firewall inventory %d times", reader.ruleCalls)
	}
}
