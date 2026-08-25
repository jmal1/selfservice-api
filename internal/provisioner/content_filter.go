package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/opnsense"
)

const (
	contentFilterRulePrefix       = "crucible:content-filter:v1:"
	ContentFilterDNSBLDescription = contentFilterRulePrefix + "dnsbl"
	contentFilterFeedBaseURL      = "https://student-filter-feed.lab.jmal.io"
	contentFilterFinalSource      = "10.100.0.0/16"
)

const contentFilterDNSBLDescription = ContentFilterDNSBLDescription

var contentFilterBypassPorts = []string{
	"500", "1080", "1194", "1701", "1723", "3128", "4500",
	"8080", "8118", "9001", "9030", "9050", "9150", "51820",
}

var contentFilterFeedPaths = []string{
	"/lists/drogue.txt",
	"/lists/agressif.txt",
	"/lists/dangerous_material.txt",
	"/lists/audio-video.txt",
	"/lists/social_networks.txt",
	"/lists/weapons.txt",
}

var contentFilterDoHDestinations = []string{
	"1.0.0.1", "1.1.1.1",
	"8.8.4.4", "8.8.8.8",
	"9.9.9.9", "149.112.112.112",
	"185.228.168.9", "185.228.169.9",
	"208.67.220.220", "208.67.222.222",
	"94.140.14.14", "94.140.15.15",
}

var contentFilterBypassDomains = []string{
	"cloudflare-dns.com",
	"dns.google",
	"dns.quad9.net",
	"doh.cleanbrowsing.org",
	"doh.opendns.com",
	"dns.adguard-dns.com",
	"dns.mullvad.net",
	"freedns.controld.com",
	"expressvpn.com",
	"mullvad.net",
	"nordvpn.com",
	"openvpn.net",
	"outline-vpn.com",
	"protonvpn.com",
	"psiphon.ca",
	"surfshark.com",
	"tailscale.com",
	"torproject.org",
	"windscribe.com",
	"zerotier.com",
}

// ContentFilterConfig is deliberately deployment-owned. There is no student or
// instructor API for modifying policy or its permanent allowlist.
type ContentFilterConfig struct {
	Enabled             bool
	Canary              bool
	SourceNetwork       string
	CategoryFeedBaseURL string
	Allowlist           []string
}

type contentFilterClient interface {
	BeginContentFilterTransaction(ctx context.Context) (context.Context, func(), error)
	GetUnboundSafeSearch(ctx context.Context) (bool, error)
	SupportsSourceScopedSafeSearch(ctx context.Context) (bool, error)
	SnapshotSourceScopedSafeSearch(ctx context.Context) (opnsense.SourceScopedSafeSearchState, error)
	ReadSourceScopedSafeSearchState(ctx context.Context) (opnsense.SourceScopedSafeSearchReadOnlyState, error)
	ConfigureSourceScopedSafeSearch(ctx context.Context, sourceNetwork string) error
	RestoreSourceScopedSafeSearch(ctx context.Context, state opnsense.SourceScopedSafeSearchState) error
	InspectSourceScopedSafeSearch(ctx context.Context, sourceNetwork string) error
	CheckStudentIPv6InternetRoute(ctx context.Context, studentInterfaces []string) error
	GetFirewallRules(ctx context.Context) ([]opnsense.FirewallRuleInfo, error)
	CreateFirewallRule(ctx context.Context, rule opnsense.FirewallRule) (string, error)
	UpdateFirewallRule(ctx context.Context, uuid string, rule opnsense.FirewallRule) error
	DeleteFirewallRule(ctx context.Context, uuid string) error
	ApplyFirewall(ctx context.Context) error
	ListDNSBLPolicies(ctx context.Context) ([]opnsense.DNSBLPolicy, error)
	GetDNSBLPolicy(ctx context.Context, uuid string) (opnsense.DNSBLPolicy, error)
	CreateDNSBLPolicy(ctx context.Context, policy opnsense.DNSBLPolicy) (string, error)
	UpdateDNSBLPolicy(ctx context.Context, uuid string, policy opnsense.DNSBLPolicy) error
	DeleteDNSBLPolicy(ctx context.Context, uuid string) error
	ActivateUnboundDNSBL(ctx context.Context, expected []opnsense.DNSBLPolicy) error
	InspectUnboundDNSBLRuntime(ctx context.Context, expected []opnsense.DNSBLPolicy) error
}

type contentFilterTransactionStore interface {
	GetContentFilterTransaction(ctx context.Context) (*database.ContentFilterTransaction, error)
	CreateContentFilterTransaction(
		ctx context.Context,
		operationID uuid.UUID,
		sourceNetwork string,
		snapshot json.RawMessage,
	) error
	CompleteContentFilterTransaction(ctx context.Context, operationID uuid.UUID) error
	ListContentFilterCanaryReservations(ctx context.Context) ([]string, error)
	ReplaceContentFilterCanaryReservations(ctx context.Context, sources []string) error
}

type contentFilterReconcileResult struct {
	ExpectedRules       int
	MissingRules        int
	DriftedRules        int
	RemovedRules        int
	ControllerReady     bool
	EffectiveReady      bool
	FirewallApplied     bool
	FirewallSafeToApply bool
}

type contentFilterSnapshot struct {
	Version            int                                  `json:"version"`
	Firewall           []opnsense.FirewallRuleInfo          `json:"firewall"`
	DNSBL              []opnsense.DNSBLPolicy               `json:"dnsbl"`
	SafeSearch         opnsense.SourceScopedSafeSearchState `json:"safesearch"`
	CanaryReservations []string                             `json:"canary_reservations"`
}

const contentFilterSnapshotVersion = 1
const contentFilterRollbackTimeout = 5 * time.Minute

func validateContentFilterConfig(cfg ContentFilterConfig) error {
	if !cfg.Enabled {
		return nil
	}
	source, err := netip.ParsePrefix(strings.TrimSpace(cfg.SourceNetwork))
	if err != nil || source.Masked().String() != strings.TrimSpace(cfg.SourceNetwork) || !source.Addr().Is4() {
		return fmt.Errorf("content filter source network must be a canonical IPv4 CIDR, got %q", cfg.SourceNetwork)
	}
	final := netip.MustParsePrefix(contentFilterFinalSource)
	if cfg.Canary {
		if source.Bits() != 24 || !final.Contains(source.Addr()) {
			return fmt.Errorf("content filter canary source must be one /24 inside %s, got %q", contentFilterFinalSource, cfg.SourceNetwork)
		}
	} else if source.String() != contentFilterFinalSource {
		return fmt.Errorf("content filter final source network must be %s, got %q", contentFilterFinalSource, cfg.SourceNetwork)
	}
	if err := validateContentFilterFeedBaseURL(cfg.CategoryFeedBaseURL); err != nil {
		return err
	}
	for _, domain := range cfg.Allowlist {
		if !validPolicyDomain(domain) {
			return fmt.Errorf("invalid permanent allowlist domain %q", domain)
		}
	}
	return nil
}

func validateContentFilterAllocations(cfg ContentFilterConfig, allocatedSubnets []string) error {
	if !cfg.Enabled {
		return nil
	}
	final := netip.MustParsePrefix(contentFilterFinalSource)
	var canary netip.Prefix
	if cfg.Canary {
		canary = netip.MustParsePrefix(cfg.SourceNetwork)
	}
	for _, raw := range allocatedSubnets {
		value := strings.TrimSpace(raw)
		allocated, err := netip.ParsePrefix(value)
		if err != nil ||
			allocated.Masked().String() != value ||
			!allocated.Addr().Is4() ||
			allocated.Bits() != 24 ||
			!final.Contains(allocated.Addr()) {
			return fmt.Errorf(
				"retained pod subnet %q must be a canonical /24 inside %s",
				raw,
				contentFilterFinalSource,
			)
		}
		if cfg.Canary && (canary.Contains(allocated.Addr()) || allocated.Contains(canary.Addr())) {
			return fmt.Errorf("content filter canary %s is retained as pod subnet %s", canary, allocated)
		}
	}
	return nil
}

func validateContentFilterFeedBaseURL(value string) error {
	feed, err := url.ParseRequestURI(strings.TrimSpace(value))
	if err != nil ||
		feed.Scheme != "https" ||
		feed.Host != "student-filter-feed.lab.jmal.io" ||
		feed.User != nil ||
		feed.RawQuery != "" ||
		feed.ForceQuery ||
		feed.Fragment != "" ||
		(feed.Path != "" && feed.Path != "/") {
		return fmt.Errorf("content filter category feed base URL must be exactly %s", contentFilterFeedBaseURL)
	}
	return nil
}

func desiredContentFilterRules(cfg ContentFilterConfig) []opnsense.FirewallRule {
	source := canonicalField(cfg.SourceNetwork)
	rules := []opnsense.FirewallRule{
		{
			Enabled: "1", Sequence: "100", Quick: "1", Action: "pass",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source, Destination: "(self)", DestinationPort: "53", Log: "0",
			Description: contentFilterRulePrefix + "dns-to-firewall",
		},
		{
			Enabled: "1", Sequence: "110", Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source, Destination: "any", DestinationPort: "53", Log: "1",
			Description: contentFilterRulePrefix + "external-dns",
		},
		{
			Enabled: "1", Sequence: "120", Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source, Destination: "any", DestinationPort: "853", Log: "1",
			Description: contentFilterRulePrefix + "dot-doq",
		},
		{
			Enabled: "1", Sequence: "121", Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "UDP",
			Source: source, Destination: "any", DestinationPort: "784", Log: "1",
			Description: contentFilterRulePrefix + "doq-784",
		},
		{
			Enabled: "1", Sequence: "122", Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "UDP",
			Source: source, Destination: "any", DestinationPort: "8853", Log: "1",
			Description: contentFilterRulePrefix + "doq-8853",
		},
		{
			Enabled: "1", Sequence: "123", Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "UDP",
			Source: source, Destination: "any", DestinationPort: "443", Log: "1",
			Description: contentFilterRulePrefix + "quic-443",
		},
		{
			Enabled: "1", Sequence: "130", Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source, Destination: strings.Join(contentFilterDoHDestinations, ","),
			DestinationPort: "443", Log: "1",
			Description: contentFilterRulePrefix + "common-doh",
		},
	}
	for i, port := range contentFilterBypassPorts {
		rules = append(rules, opnsense.FirewallRule{
			Enabled: "1", Sequence: fmt.Sprintf("%d", 140+i), Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source, DestinationInvert: "1", Destination: contentFilterFinalSource,
			DestinationPort: port, Log: "1",
			Description: contentFilterRulePrefix + "bypass-port-" + port,
		})
	}
	for i, protocol := range []string{"GRE", "ESP", "AH"} {
		rules = append(rules, opnsense.FirewallRule{
			Enabled: "1", Sequence: fmt.Sprintf("%d", 160+i), Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: protocol,
			Source: source, DestinationInvert: "1", Destination: contentFilterFinalSource, Log: "1",
			Description: contentFilterRulePrefix + "vpn-" + strings.ToLower(protocol),
		})
	}
	return rules
}

func reconcileContentFilter(
	ctx context.Context,
	opn contentFilterClient,
	store contentFilterTransactionStore,
	allocatedSubnets []string,
	activeInterfaces []string,
	cfg ContentFilterConfig,
) (result contentFilterReconcileResult, retErr error) {
	if err := validateContentFilterConfig(cfg); err != nil {
		return result, err
	}
	if !cfg.Enabled {
		return inspectDisabledContentFilter(ctx, opn, store)
	}
	if store == nil {
		return result, errors.New("content-filter transaction store is unavailable")
	}
	transactionCtx, release, err := opn.BeginContentFilterTransaction(ctx)
	if err != nil {
		return result, fmt.Errorf("acquire content-filter transaction lock: %w", err)
	}
	defer release()
	ctx = transactionCtx

	if err := requireGlobalSafeSearchDisabled(ctx, opn); err != nil {
		return result, err
	}
	if err := recoverContentFilterTransaction(ctx, opn, store); err != nil {
		return result, fmt.Errorf("recover interrupted content-filter transaction: %w", err)
	}
	if err := requireGlobalSafeSearchDisabled(ctx, opn); err != nil {
		return result, err
	}
	if err := validateContentFilterAllocations(cfg, allocatedSubnets); err != nil {
		return result, err
	}
	supported, err := opn.SupportsSourceScopedSafeSearch(ctx)
	if err != nil {
		return result, fmt.Errorf("check source-scoped SafeSearch support: %w", err)
	}
	if !supported {
		return result, fmt.Errorf("content filter activation blocked: source-scoped SafeSearch transactional ownership is unavailable")
	}
	if err := opn.CheckStudentIPv6InternetRoute(ctx, activeInterfaces); err != nil {
		return result, fmt.Errorf("content filter activation blocked by student IPv6 routing: %w", err)
	}
	previousReservations, err := store.ListContentFilterCanaryReservations(ctx)
	if err != nil {
		return result, fmt.Errorf("snapshot content-filter canary reservations: %w", err)
	}
	sort.Strings(previousReservations)
	desiredReservations := []string(nil)
	if cfg.Canary {
		desiredReservations = []string{cfg.SourceNetwork}
	}
	stagedReservations := canonicalStringSet(append(
		append([]string(nil), previousReservations...),
		desiredReservations...,
	))

	result.ExpectedRules = len(desiredContentFilterRules(cfg))
	snapshot, err := snapshotContentFilterState(ctx, opn)
	if err != nil {
		return result, err
	}
	snapshot.CanaryReservations = previousReservations
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return result, fmt.Errorf("encode content-filter recovery snapshot: %w", err)
	}
	operationID := uuid.New()
	if err := store.CreateContentFilterTransaction(ctx, operationID, cfg.SourceNetwork, snapshotJSON); err != nil {
		return result, fmt.Errorf("persist content-filter recovery snapshot: %w", err)
	}
	rollbackRequired := false
	defer func() {
		if retErr == nil || !rollbackRequired {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), contentFilterRollbackTimeout)
		defer cancel()
		if rollbackErr := restoreContentFilterState(rollbackCtx, opn, store, snapshot); rollbackErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("content-filter rollback incomplete: %w", rollbackErr))
		} else {
			if completeErr := store.CompleteContentFilterTransaction(rollbackCtx, operationID); completeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("content-filter rollback journal cleanup failed: %w", completeErr))
			} else {
				retErr = fmt.Errorf("%w; rollback completed and verified", retErr)
			}
		}
	}()
	rollbackRequired = true
	if err := store.ReplaceContentFilterCanaryReservations(ctx, stagedReservations); err != nil {
		return result, fmt.Errorf("stage content-filter canary reservation: %w", err)
	}

	firewallInventory, err := opn.GetFirewallRules(ctx)
	if err != nil {
		return result, fmt.Errorf("refresh firewall inventory before staging: %w", err)
	}
	firewallPlan, err := planContentFilterFirewall(firewallInventory, cfg)
	if err != nil {
		return result, err
	}
	result.MissingRules = firewallPlan.missing
	result.DriftedRules = firewallPlan.drifted
	result.RemovedRules = firewallPlan.removed
	if err := applyContentFilterFirewallPlan(ctx, opn, firewallPlan); err != nil {
		return result, err
	}

	dnsPolicies, err := listOwnedContentFilterDNSBL(ctx, opn)
	if err != nil {
		return result, err
	}
	dnsModelMutated, err := stageContentFilterDNSBL(ctx, opn, dnsPolicies, cfg)
	if err != nil {
		return result, err
	}

	stagedRules, stagedDNS, err := readContentFilterModel(ctx, opn)
	if err != nil {
		return result, err
	}
	if err := InspectContentFilterPolicy(stagedRules, stagedDNS, cfg); err != nil {
		return result, fmt.Errorf("validate staged content-filter model: %w", err)
	}
	dnsRuntimeReady := false
	if !dnsModelMutated {
		dnsRuntimeReady = opn.InspectUnboundDNSBLRuntime(ctx, stagedDNS) == nil
	}

	if err := opn.ConfigureSourceScopedSafeSearch(ctx, cfg.SourceNetwork); err != nil {
		return result, fmt.Errorf("configure source-scoped SafeSearch: %w", err)
	}
	if err := requireGlobalSafeSearchDisabled(ctx, opn); err != nil {
		return result, err
	}
	if firewallPlan.mutated {
		if err := opn.ApplyFirewall(ctx); err != nil {
			return result, fmt.Errorf("apply content-filter firewall policy: %w", err)
		}
		result.FirewallApplied = true
	}

	_, runtimeDNS, err := readContentFilterModel(ctx, opn)
	if err != nil {
		return result, err
	}
	if err := requireGlobalSafeSearchDisabled(ctx, opn); err != nil {
		return result, err
	}
	if dnsModelMutated || !dnsRuntimeReady {
		if err := opn.ActivateUnboundDNSBL(ctx, runtimeDNS); err != nil {
			return result, fmt.Errorf("activate content-filter DNSBL: %w", err)
		}
	}

	readbackRules, readbackDNS, err := readContentFilterModel(ctx, opn)
	if err != nil {
		return result, err
	}
	if err := InspectContentFilterPolicy(readbackRules, readbackDNS, cfg); err != nil {
		return result, fmt.Errorf("read back content-filter model: %w", err)
	}
	if err := opn.InspectSourceScopedSafeSearch(ctx, cfg.SourceNetwork); err != nil {
		return result, fmt.Errorf("read back source-scoped SafeSearch: %w", err)
	}
	if err := opn.InspectUnboundDNSBLRuntime(ctx, readbackDNS); err != nil {
		return result, fmt.Errorf("read back DNSBL runtime: %w", err)
	}
	if err := requireGlobalSafeSearchDisabled(ctx, opn); err != nil {
		return result, err
	}
	if err := store.ReplaceContentFilterCanaryReservations(ctx, desiredReservations); err != nil {
		return result, fmt.Errorf("commit content-filter canary reservation: %w", err)
	}
	if err := store.CompleteContentFilterTransaction(ctx, operationID); err != nil {
		return result, fmt.Errorf("commit content-filter recovery journal: %w", err)
	}

	rollbackRequired = false
	result.ControllerReady = true
	result.EffectiveReady = false
	result.FirewallSafeToApply = true
	return result, nil
}

func inspectDisabledContentFilter(
	ctx context.Context,
	opn contentFilterClient,
	store contentFilterTransactionStore,
) (contentFilterReconcileResult, error) {
	var result contentFilterReconcileResult
	if store == nil {
		return result, errors.New("content-filter transaction store is unavailable")
	}
	transaction, err := store.GetContentFilterTransaction(ctx)
	if err != nil {
		return result, err
	}
	rules, err := opn.GetFirewallRules(ctx)
	if err != nil {
		return result, err
	}
	ownedRules := 0
	for _, rule := range rules {
		managed, ambiguous := classifyContentFilterFirewallRule(rule)
		if ambiguous {
			return result, fmt.Errorf("content filter is disabled but ambiguous owned-looking firewall rule %q remains", rule.UUID)
		}
		if managed {
			ownedRules++
		}
	}
	if ownedRules == 0 {
		// Applying unrelated generated pod rules cannot activate a disabled
		// content policy once exact-owned firewall state is proven absent.
		result.FirewallSafeToApply = true
	}
	policies, err := listOwnedContentFilterDNSBL(ctx, opn)
	if err != nil {
		return result, err
	}
	safeSearch, err := opn.ReadSourceScopedSafeSearchState(ctx)
	if err != nil {
		return result, fmt.Errorf("inspect disabled source-scoped SafeSearch state: %w", err)
	}
	globalSafeSearch, err := opn.GetUnboundSafeSearch(ctx)
	if err != nil {
		return result, fmt.Errorf("inspect disabled global SafeSearch state: %w", err)
	}
	reservations, err := store.ListContentFilterCanaryReservations(ctx)
	if err != nil {
		return result, fmt.Errorf("inspect disabled content-filter canary reservations: %w", err)
	}
	if transaction != nil ||
		ownedRules != 0 ||
		len(policies) != 0 ||
		len(reservations) != 0 ||
		safeSearch.State.Exists ||
		safeSearch.RecoveryPending ||
		globalSafeSearch {
		return result, fmt.Errorf(
			"content filter is disabled but state remains: journal=%t firewall=%d dnsbl=%d canary_reservations=%d source_scoped_safesearch=%t safesearch_recovery=%t global_safesearch=%t",
			transaction != nil,
			ownedRules,
			len(policies),
			len(reservations),
			safeSearch.State.Exists,
			safeSearch.RecoveryPending,
			globalSafeSearch,
		)
	}
	result.ControllerReady = true
	result.EffectiveReady = false
	return result, nil
}

func requireGlobalSafeSearchDisabled(ctx context.Context, opn contentFilterClient) error {
	enabled, err := opn.GetUnboundSafeSearch(ctx)
	if err != nil {
		return fmt.Errorf("inspect global Unbound SafeSearch: %w", err)
	}
	if enabled {
		return errors.New("content filter activation blocked: global Unbound SafeSearch must remain disabled")
	}
	return nil
}

func recoverContentFilterTransaction(
	ctx context.Context,
	opn contentFilterClient,
	store contentFilterTransactionStore,
) error {
	transaction, err := store.GetContentFilterTransaction(ctx)
	if err != nil {
		return err
	}
	if transaction == nil {
		return nil
	}
	var snapshot contentFilterSnapshot
	if err := json.Unmarshal(transaction.Snapshot, &snapshot); err != nil {
		return fmt.Errorf("decode operation %s snapshot: %w", transaction.OperationID, err)
	}
	if snapshot.Version != contentFilterSnapshotVersion {
		return fmt.Errorf(
			"operation %s snapshot version is %d, want %d",
			transaction.OperationID,
			snapshot.Version,
			contentFilterSnapshotVersion,
		)
	}
	if err := restoreContentFilterState(ctx, opn, store, snapshot); err != nil {
		return fmt.Errorf("restore operation %s: %w", transaction.OperationID, err)
	}
	if err := store.CompleteContentFilterTransaction(ctx, transaction.OperationID); err != nil {
		return fmt.Errorf("complete recovered operation %s: %w", transaction.OperationID, err)
	}
	return nil
}

type contentFilterFirewallPlan struct {
	create  []opnsense.FirewallRule
	update  map[string]opnsense.FirewallRule
	delete  []string
	missing int
	drifted int
	removed int
	mutated bool
}

func planContentFilterFirewall(inventory []opnsense.FirewallRuleInfo, cfg ContentFilterConfig) (contentFilterFirewallPlan, error) {
	var plan contentFilterFirewallPlan
	plan.update = make(map[string]opnsense.FirewallRule)
	desired := desiredContentFilterRules(cfg)
	desiredByDescription := make(map[string]opnsense.FirewallRule, len(desired))
	for _, rule := range desired {
		desiredByDescription[rule.Description] = rule
	}
	owned := make(map[string][]opnsense.FirewallRuleInfo)
	for _, current := range inventory {
		managed, ambiguous := classifyContentFilterFirewallRule(current)
		if ambiguous {
			return plan, fmt.Errorf("ambiguous content-filter firewall rule %q; refusing mutation", current.UUID)
		}
		if managed {
			owned[current.Description] = append(owned[current.Description], current)
		}
	}
	for description := range owned {
		sort.Slice(owned[description], func(i, j int) bool {
			return owned[description][i].UUID < owned[description][j].UUID
		})
	}
	for description, candidates := range owned {
		want, desiredRule := desiredByDescription[description]
		if !desiredRule {
			for _, candidate := range candidates {
				plan.delete = append(plan.delete, candidate.UUID)
				plan.removed++
			}
			continue
		}
		if !equivalentContentFilterRule(candidates[0], want) {
			plan.update[candidates[0].UUID] = want
			plan.drifted++
		}
		for _, duplicate := range candidates[1:] {
			plan.delete = append(plan.delete, duplicate.UUID)
			plan.removed++
		}
	}
	for description, want := range desiredByDescription {
		if len(owned[description]) == 0 {
			plan.create = append(plan.create, want)
			plan.missing++
		}
	}
	sort.Slice(plan.create, func(i, j int) bool { return plan.create[i].Description < plan.create[j].Description })
	sort.Strings(plan.delete)
	plan.mutated = len(plan.create) > 0 || len(plan.update) > 0 || len(plan.delete) > 0
	return plan, nil
}

func applyContentFilterFirewallPlan(ctx context.Context, opn contentFilterClient, plan contentFilterFirewallPlan) error {
	updateUUIDs := make([]string, 0, len(plan.update))
	for uuid := range plan.update {
		updateUUIDs = append(updateUUIDs, uuid)
	}
	sort.Strings(updateUUIDs)
	for _, uuid := range updateUUIDs {
		if err := opn.UpdateFirewallRule(ctx, uuid, plan.update[uuid]); err != nil {
			return fmt.Errorf("update owned content-filter firewall rule %s: %w", uuid, err)
		}
	}
	for _, rule := range plan.create {
		if _, err := opn.CreateFirewallRule(ctx, rule); err != nil {
			return fmt.Errorf("create owned content-filter firewall rule %q: %w", rule.Description, err)
		}
	}
	for _, uuid := range plan.delete {
		if err := opn.DeleteFirewallRule(ctx, uuid); err != nil {
			return fmt.Errorf("delete owned content-filter firewall rule %s: %w", uuid, err)
		}
	}
	return nil
}

func snapshotContentFilterState(ctx context.Context, opn contentFilterClient) (contentFilterSnapshot, error) {
	rules, err := opn.GetFirewallRules(ctx)
	if err != nil {
		return contentFilterSnapshot{}, fmt.Errorf("snapshot firewall state: %w", err)
	}
	var ownedRules []opnsense.FirewallRuleInfo
	for _, rule := range rules {
		managed, ambiguous := classifyContentFilterFirewallRule(rule)
		if ambiguous {
			return contentFilterSnapshot{}, fmt.Errorf("snapshot firewall state: ambiguous content-filter rule %q", rule.UUID)
		}
		if managed {
			ownedRules = append(ownedRules, rule)
		}
	}
	dnsbl, err := listOwnedContentFilterDNSBL(ctx, opn)
	if err != nil {
		return contentFilterSnapshot{}, fmt.Errorf("snapshot DNSBL state: %w", err)
	}
	safesearch, err := opn.SnapshotSourceScopedSafeSearch(ctx)
	if err != nil {
		return contentFilterSnapshot{}, fmt.Errorf("snapshot SafeSearch state: %w", err)
	}
	return contentFilterSnapshot{
		Version:    contentFilterSnapshotVersion,
		Firewall:   ownedRules,
		DNSBL:      dnsbl,
		SafeSearch: safesearch,
	}, nil
}

func restoreContentFilterState(
	ctx context.Context,
	opn contentFilterClient,
	store contentFilterTransactionStore,
	snapshot contentFilterSnapshot,
) error {
	if snapshot.Version != contentFilterSnapshotVersion {
		return fmt.Errorf("unsupported content-filter snapshot version %d", snapshot.Version)
	}
	var errs []error
	if err := requireGlobalSafeSearchDisabled(ctx, opn); err != nil {
		errs = append(errs, fmt.Errorf("refuse rollback activation while global SafeSearch is enabled: %w", err))
		return errors.Join(errs...)
	}
	if err := restoreContentFilterDNSBLModel(ctx, opn, snapshot.DNSBL); err != nil {
		errs = append(errs, err)
	}
	if err := opn.RestoreSourceScopedSafeSearch(ctx, snapshot.SafeSearch); err != nil {
		errs = append(errs, fmt.Errorf("restore SafeSearch state: %w", err))
	} else if current, err := opn.SnapshotSourceScopedSafeSearch(ctx); err != nil {
		errs = append(errs, fmt.Errorf("read restored SafeSearch state: %w", err))
	} else if current.Exists != snapshot.SafeSearch.Exists || !bytes.Equal(current.Content, snapshot.SafeSearch.Content) {
		errs = append(errs, errors.New("restored SafeSearch state does not match snapshot"))
	}
	if err := restoreContentFilterFirewallModel(ctx, opn, snapshot.Firewall); err != nil {
		errs = append(errs, err)
	} else if err := requireGlobalSafeSearchDisabled(ctx, opn); err != nil {
		errs = append(errs, fmt.Errorf("refuse restored firewall activation: %w", err))
	} else if err := opn.ApplyFirewall(ctx); err != nil {
		errs = append(errs, fmt.Errorf("apply restored firewall state: %w", err))
	} else if current, err := opn.GetFirewallRules(ctx); err != nil {
		errs = append(errs, fmt.Errorf("read applied firewall rollback state: %w", err))
	} else if err := compareContentFilterFirewallSnapshot(current, snapshot.Firewall); err != nil {
		errs = append(errs, fmt.Errorf("verify applied firewall rollback state: %w", err))
	}
	restoredDNS, err := listOwnedContentFilterDNSBL(ctx, opn)
	if err != nil {
		errs = append(errs, fmt.Errorf("read restored DNSBL model: %w", err))
	} else if err := compareContentFilterDNSBLSnapshot(restoredDNS, snapshot.DNSBL); err != nil {
		errs = append(errs, fmt.Errorf("verify restored DNSBL model: %w", err))
	} else if err := requireGlobalSafeSearchDisabled(ctx, opn); err != nil {
		errs = append(errs, fmt.Errorf("refuse restored DNSBL activation: %w", err))
	} else if err := opn.ActivateUnboundDNSBL(ctx, restoredDNS); err != nil {
		errs = append(errs, fmt.Errorf("activate restored DNSBL state: %w", err))
	} else if err := opn.InspectUnboundDNSBLRuntime(ctx, restoredDNS); err != nil {
		errs = append(errs, fmt.Errorf("verify restored DNSBL runtime: %w", err))
	}
	if len(errs) == 0 {
		if err := store.ReplaceContentFilterCanaryReservations(ctx, snapshot.CanaryReservations); err != nil {
			errs = append(errs, fmt.Errorf("restore content-filter canary reservations: %w", err))
		}
	}
	return errors.Join(errs...)
}

func requireContentFilterFirewallMutationSafe(
	ctx context.Context,
	store interface {
		GetContentFilterTransaction(context.Context) (*database.ContentFilterTransaction, error)
	},
	opn interface {
		GetFirewallRules(context.Context) ([]opnsense.FirewallRuleInfo, error)
	},
) error {
	transaction, err := store.GetContentFilterTransaction(ctx)
	if err != nil {
		return fmt.Errorf("inspect content-filter recovery journal: %w", err)
	}
	if transaction == nil {
		return nil
	}
	rules, err := opn.GetFirewallRules(ctx)
	if err != nil {
		return fmt.Errorf(
			"content-filter operation %s requires recovery and firewall safety could not be inspected: %w",
			transaction.OperationID,
			err,
		)
	}
	for _, rule := range rules {
		managed, ambiguous := classifyContentFilterFirewallRule(rule)
		if ambiguous || managed {
			return fmt.Errorf(
				"content-filter operation %s requires recovery before firewall mutation",
				transaction.OperationID,
			)
		}
	}
	return nil
}

func restoreContentFilterFirewallModel(
	ctx context.Context,
	opn contentFilterClient,
	snapshot []opnsense.FirewallRuleInfo,
) error {
	current, err := opn.GetFirewallRules(ctx)
	if err != nil {
		return fmt.Errorf("read firewall state for rollback: %w", err)
	}
	for _, rule := range current {
		managed, ambiguous := classifyContentFilterFirewallRule(rule)
		if ambiguous {
			return fmt.Errorf("rollback found ambiguous content-filter firewall rule %q", rule.UUID)
		}
		if managed {
			if err := opn.DeleteFirewallRule(ctx, rule.UUID); err != nil {
				return fmt.Errorf("remove current owned firewall rule %s during rollback: %w", rule.UUID, err)
			}
		}
	}
	for _, rule := range snapshot {
		if _, err := opn.CreateFirewallRule(ctx, firewallRuleFromInfo(rule)); err != nil {
			return fmt.Errorf("restore owned firewall rule %q: %w", rule.Description, err)
		}
	}
	current, err = opn.GetFirewallRules(ctx)
	if err != nil {
		return fmt.Errorf("read restored firewall model: %w", err)
	}
	return compareContentFilterFirewallSnapshot(current, snapshot)
}

func restoreContentFilterDNSBLModel(
	ctx context.Context,
	opn contentFilterClient,
	snapshot []opnsense.DNSBLPolicy,
) error {
	current, err := listOwnedContentFilterDNSBL(ctx, opn)
	if err != nil {
		return fmt.Errorf("read DNSBL state for rollback: %w", err)
	}
	for _, policy := range current {
		if err := opn.DeleteDNSBLPolicy(ctx, policy.UUID); err != nil {
			return fmt.Errorf("remove current owned DNSBL policy %s during rollback: %w", policy.UUID, err)
		}
	}
	for _, policy := range snapshot {
		if _, err := opn.CreateDNSBLPolicy(ctx, dnsblPolicyWithoutUUID(policy)); err != nil {
			return fmt.Errorf("restore owned DNSBL policy %q: %w", policy.Description, err)
		}
	}
	current, err = listOwnedContentFilterDNSBL(ctx, opn)
	if err != nil {
		return fmt.Errorf("read restored DNSBL model: %w", err)
	}
	return compareContentFilterDNSBLSnapshot(current, snapshot)
}

func compareContentFilterFirewallSnapshot(
	inventory []opnsense.FirewallRuleInfo,
	snapshot []opnsense.FirewallRuleInfo,
) error {
	var current []opnsense.FirewallRuleInfo
	for _, rule := range inventory {
		managed, ambiguous := classifyContentFilterFirewallRule(rule)
		if ambiguous {
			return fmt.Errorf("ambiguous content-filter firewall rule %q", rule.UUID)
		}
		if managed {
			current = append(current, rule)
		}
	}
	if len(current) != len(snapshot) {
		return fmt.Errorf("owned firewall rule count is %d, want %d", len(current), len(snapshot))
	}
	used := make([]bool, len(current))
	for _, want := range snapshot {
		match := -1
		for i, candidate := range current {
			if !used[i] && equivalentContentFilterRule(candidate, firewallRuleFromInfo(want)) {
				match = i
				break
			}
		}
		if match < 0 {
			return fmt.Errorf("owned firewall rule %q was not restored exactly", want.Description)
		}
		used[match] = true
	}
	return nil
}

func compareContentFilterDNSBLSnapshot(current, snapshot []opnsense.DNSBLPolicy) error {
	if len(current) != len(snapshot) {
		return fmt.Errorf("owned DNSBL policy count is %d, want %d", len(current), len(snapshot))
	}
	used := make([]bool, len(current))
	for _, want := range snapshot {
		match := -1
		for i, candidate := range current {
			if !used[i] && equivalentDNSBLPolicy(candidate, want) {
				match = i
				break
			}
		}
		if match < 0 {
			return fmt.Errorf("owned DNSBL policy %q was not restored exactly", want.Description)
		}
		used[match] = true
	}
	return nil
}

func stageContentFilterDNSBL(
	ctx context.Context,
	opn contentFilterClient,
	current []opnsense.DNSBLPolicy,
	cfg ContentFilterConfig,
) (bool, error) {
	desired := desiredDNSBLPolicy(cfg)
	sort.Slice(current, func(i, j int) bool { return current[i].UUID < current[j].UUID })
	switch {
	case len(current) == 0:
		if _, err := opn.CreateDNSBLPolicy(ctx, desired); err != nil {
			return false, fmt.Errorf("create owned content-filter DNSBL policy: %w", err)
		}
		return true, nil
	default:
		mutated := false
		if !equivalentDNSBLPolicy(current[0], desired) {
			if err := opn.UpdateDNSBLPolicy(ctx, current[0].UUID, desired); err != nil {
				return false, fmt.Errorf("update owned content-filter DNSBL policy %s: %w", current[0].UUID, err)
			}
			mutated = true
		}
		for _, duplicate := range current[1:] {
			if err := opn.DeleteDNSBLPolicy(ctx, duplicate.UUID); err != nil {
				return false, fmt.Errorf("delete duplicate content-filter DNSBL policy %s: %w", duplicate.UUID, err)
			}
			mutated = true
		}
		return mutated, nil
	}
}

func listOwnedContentFilterDNSBL(ctx context.Context, opn contentFilterClient) ([]opnsense.DNSBLPolicy, error) {
	rows, err := opn.ListDNSBLPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list content-filter DNSBL policies: %w", err)
	}
	var policies []opnsense.DNSBLPolicy
	for _, row := range rows {
		description := strings.TrimSpace(row.Description)
		if !strings.HasPrefix(description, contentFilterRulePrefix) {
			continue
		}
		if description != contentFilterDNSBLDescription {
			return nil, fmt.Errorf("ambiguous owned DNSBL description %q; refusing mutation", description)
		}
		current, err := opn.GetDNSBLPolicy(ctx, row.UUID)
		if err != nil {
			return nil, fmt.Errorf("get content-filter DNSBL policy %s: %w", row.UUID, err)
		}
		if !safeOwnedDNSBLPolicy(current) {
			return nil, fmt.Errorf("ambiguous content-filter DNSBL policy %s; refusing mutation", row.UUID)
		}
		policies = append(policies, current)
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].UUID < policies[j].UUID })
	return policies, nil
}

func readContentFilterModel(
	ctx context.Context,
	opn contentFilterClient,
) ([]opnsense.FirewallRuleInfo, []opnsense.DNSBLPolicy, error) {
	rules, err := opn.GetFirewallRules(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read content-filter firewall model: %w", err)
	}
	policies, err := listOwnedContentFilterDNSBL(ctx, opn)
	if err != nil {
		return nil, nil, err
	}
	return rules, policies, nil
}

func contentFilterFeedListURLs(baseURL string) string {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	urls := make([]string, 0, len(contentFilterFeedPaths))
	for _, path := range contentFilterFeedPaths {
		urls = append(urls, baseURL+path)
	}
	return strings.Join(urls, ",")
}

func desiredDNSBLPolicy(cfg ContentFilterConfig) opnsense.DNSBLPolicy {
	allowlist := append([]string(nil), cfg.Allowlist...)
	for i := range allowlist {
		allowlist[i] = canonicalField(allowlist[i])
	}
	sort.Strings(allowlist)
	bypassDomains := append([]string(nil), contentFilterBypassDomains...)
	sort.Strings(bypassDomains)
	return opnsense.DNSBLPolicy{
		Enabled:     "1",
		Types:       "hgz014,hgz021,oisd2",
		Lists:       contentFilterFeedListURLs(cfg.CategoryFeedBaseURL),
		Allowlists:  strings.Join(allowlist, ","),
		Wildcards:   strings.Join(bypassDomains, ","),
		SourceNets:  canonicalField(cfg.SourceNetwork),
		NXDomain:    "1",
		CacheTTL:    "3600",
		Description: contentFilterDNSBLDescription,
	}
}

func classifyContentFilterFirewallRule(rule opnsense.FirewallRuleInfo) (managed bool, ambiguous bool) {
	description := strings.TrimSpace(rule.Description)
	if !strings.HasPrefix(description, contentFilterRulePrefix) {
		return false, false
	}
	if rule.UUID == "" ||
		canonicalInterfaceList(rule.Interface) != "" ||
		isTruthyFirewallField(rule.InterfaceInvert) ||
		canonicalField(rule.Direction) != "in" ||
		canonicalField(rule.IPProtocol) != "inet" ||
		isTruthyFirewallField(rule.SourceInvert) ||
		canonicalField(rule.SourcePort) != "" ||
		!safeContentFilterAdvancedFirewallFields(rule.Advanced) {
		return false, true
	}

	source, err := netip.ParsePrefix(canonicalField(rule.Source))
	if err != nil || !allowedHistoricalContentFilterSource(source) {
		return false, true
	}
	sequence, err := strconv.Atoi(canonicalField(rule.Sequence))
	if err != nil || sequence < 1 || sequence >= 1000 {
		return false, true
	}
	if canonicalField(rule.Action) == "pass" {
		if canonicalField(rule.Destination) != "(self)" ||
			canonicalCSV(rule.DestinationPort) != "53" ||
			canonicalField(rule.Protocol) != "tcp/udp" {
			return false, true
		}
	} else if canonicalField(rule.Action) != "block" {
		return false, true
	}
	return true, false
}

func safeContentFilterAdvancedFirewallFields(fields map[string]string) bool {
	booleanDefaults := map[string]bool{
		"disablereplyto": true,
		"allowopts":      true,
		"nosync":         true,
		"nopfsync":       true,
		"tcpflags_any":   true,
	}
	emptyDefaults := map[string]bool{
		"state-policy":         true,
		"icmptype":             true,
		"icmp6type":            true,
		"divert-to":            true,
		"gateway":              true,
		"replyto":              true,
		"statetimeout":         true,
		"udp-first":            true,
		"udp-multiple":         true,
		"udp-single":           true,
		"max-src-nodes":        true,
		"max-src-states":       true,
		"max-src-conn":         true,
		"max":                  true,
		"max-src-conn-rate":    true,
		"max-src-conn-rates":   true,
		"max-pkt-rate-number":  true,
		"max-pkt-rate-seconds": true,
		"overload":             true,
		"adaptivestart":        true,
		"adaptiveend":          true,
		"prio":                 true,
		"set-prio":             true,
		"set-prio-low":         true,
		"tag":                  true,
		"tagged":               true,
		"tcpflags1":            true,
		"tcpflags2":            true,
		"categories":           true,
		"sched":                true,
		"tos":                  true,
		"shaper1":              true,
		"shaper2":              true,
	}
	for key, raw := range fields {
		value := canonicalField(raw)
		switch {
		case key == "statetype":
			if value != "" && value != "keep" {
				return false
			}
		case booleanDefaults[key]:
			if value != "" && value != "0" && value != "false" {
				return false
			}
		case emptyDefaults[key]:
			if value != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func safeOwnedDNSBLPolicy(policy opnsense.DNSBLPolicy) bool {
	if strings.TrimSpace(policy.UUID) == "" || strings.TrimSpace(policy.Description) != contentFilterDNSBLDescription {
		return false
	}
	parts := strings.Split(policy.SourceNets, ",")
	if len(parts) != 1 {
		return false
	}
	source, err := netip.ParsePrefix(strings.TrimSpace(parts[0]))
	return err == nil && allowedHistoricalContentFilterSource(source)
}

func allowedHistoricalContentFilterSource(source netip.Prefix) bool {
	source = source.Masked()
	final := netip.MustParsePrefix(contentFilterFinalSource)
	return source.String() == contentFilterFinalSource ||
		(source.Bits() == 24 && final.Contains(source.Addr()))
}

func firewallRuleFromInfo(info opnsense.FirewallRuleInfo) opnsense.FirewallRule {
	return opnsense.FirewallRule{
		Enabled:           info.Enabled,
		Sequence:          info.Sequence,
		Quick:             info.Quick,
		Action:            info.Action,
		Interface:         info.Interface,
		InterfaceInvert:   info.InterfaceInvert,
		Direction:         info.Direction,
		IPProtocol:        info.IPProtocol,
		Protocol:          info.Protocol,
		SourceInvert:      info.SourceInvert,
		Source:            info.Source,
		SourcePort:        info.SourcePort,
		DestinationInvert: info.DestinationInvert,
		Destination:       info.Destination,
		DestinationPort:   info.DestinationPort,
		Log:               info.Log,
		Description:       info.Description,
		Advanced:          cloneStringMap(info.Advanced),
	}
}

func dnsblPolicyWithoutUUID(policy opnsense.DNSBLPolicy) opnsense.DNSBLPolicy {
	policy.UUID = ""
	return policy
}

func equivalentContentFilterRule(current opnsense.FirewallRuleInfo, desired opnsense.FirewallRule) bool {
	if !safeContentFilterAdvancedFirewallFields(current.Advanced) {
		return false
	}
	if desired.Advanced != nil && !equalStringMap(current.Advanced, desired.Advanced) {
		return false
	}
	return canonicalField(current.Enabled) == canonicalField(desired.Enabled) &&
		canonicalField(current.Sequence) == canonicalField(desired.Sequence) &&
		equivalentFirewallBoolean(current.Quick, desired.Quick) &&
		canonicalInterfaceList(current.Interface) == canonicalInterfaceList(desired.Interface) &&
		equivalentFirewallBoolean(current.InterfaceInvert, desired.InterfaceInvert) &&
		canonicalField(current.Action) == canonicalField(desired.Action) &&
		canonicalField(current.Direction) == canonicalField(desired.Direction) &&
		canonicalField(current.IPProtocol) == canonicalField(desired.IPProtocol) &&
		canonicalField(current.Protocol) == canonicalField(desired.Protocol) &&
		canonicalCSV(current.Source) == canonicalCSV(desired.Source) &&
		canonicalCSV(current.SourcePort) == canonicalCSV(desired.SourcePort) &&
		canonicalCSV(current.Destination) == canonicalCSV(desired.Destination) &&
		canonicalCSV(current.DestinationPort) == canonicalCSV(desired.DestinationPort) &&
		equivalentFirewallBoolean(current.SourceInvert, desired.SourceInvert) &&
		equivalentFirewallBoolean(current.DestinationInvert, desired.DestinationInvert) &&
		equivalentFirewallBoolean(current.Log, desired.Log) &&
		strings.TrimSpace(current.Description) == desired.Description
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func equalStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func equivalentDNSBLPolicy(current, desired opnsense.DNSBLPolicy) bool {
	return canonicalField(current.Enabled) == canonicalField(desired.Enabled) &&
		canonicalCSV(current.Types) == canonicalCSV(desired.Types) &&
		canonicalCSV(current.Lists) == canonicalCSV(desired.Lists) &&
		canonicalCSV(current.Allowlists) == canonicalCSV(desired.Allowlists) &&
		canonicalCSV(current.Blocklists) == canonicalCSV(desired.Blocklists) &&
		canonicalCSV(current.Wildcards) == canonicalCSV(desired.Wildcards) &&
		canonicalCSV(current.SourceNets) == canonicalCSV(desired.SourceNets) &&
		canonicalField(current.Address) == canonicalField(desired.Address) &&
		canonicalField(current.NXDomain) == canonicalField(desired.NXDomain) &&
		canonicalField(current.CacheTTL) == canonicalField(desired.CacheTTL) &&
		strings.TrimSpace(current.Description) == strings.TrimSpace(desired.Description)
}

func canonicalCSV(value string) string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = canonicalField(part); part != "" {
			out = append(out, part)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func canonicalStringSet(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func validPolicyDomain(value string) bool {
	value = canonicalField(value)
	if value == "" || strings.ContainsAny(value, "/:@ ") || !strings.Contains(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// InspectContentFilterPolicy performs the same canonical comparison as the
// controller without mutating OPNsense. Effective runtime verification remains
// a separate synthetic responsibility.
func InspectContentFilterPolicy(
	inventory []opnsense.FirewallRuleInfo,
	dnsPolicies []opnsense.DNSBLPolicy,
	cfg ContentFilterConfig,
) error {
	if err := validateContentFilterConfig(cfg); err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	desiredRules := desiredContentFilterRules(cfg)
	desiredByDescription := make(map[string]opnsense.FirewallRule, len(desiredRules))
	for _, desired := range desiredRules {
		desiredByDescription[desired.Description] = desired
	}
	ownedCount := 0
	for _, current := range inventory {
		managed, ambiguous := classifyContentFilterFirewallRule(current)
		if ambiguous {
			return fmt.Errorf("ambiguous content-filter firewall rule %q is present", current.UUID)
		}
		if !managed {
			continue
		}
		ownedCount++
		if _, desired := desiredByDescription[current.Description]; !desired {
			return fmt.Errorf("stale owned content-filter rule %q is present", current.Description)
		}
	}
	if ownedCount != len(desiredRules) {
		return fmt.Errorf("content-filter owned rule count is %d, want %d", ownedCount, len(desiredRules))
	}
	for _, desired := range desiredRules {
		var matches []opnsense.FirewallRuleInfo
		for _, current := range inventory {
			if current.Description == desired.Description {
				matches = append(matches, current)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("content-filter rule %q count is %d, want 1", desired.Description, len(matches))
		}
		if !equivalentContentFilterRule(matches[0], desired) {
			return fmt.Errorf("content-filter rule %q is drifted", desired.Description)
		}
	}
	var owned []opnsense.DNSBLPolicy
	for _, policy := range dnsPolicies {
		if strings.HasPrefix(strings.TrimSpace(policy.Description), contentFilterRulePrefix) {
			if !safeOwnedDNSBLPolicy(policy) {
				return fmt.Errorf("ambiguous content-filter DNSBL policy %q", policy.UUID)
			}
			owned = append(owned, policy)
		}
	}
	if len(owned) != 1 {
		return fmt.Errorf("content-filter DNSBL policy count is %d, want 1", len(owned))
	}
	if !equivalentDNSBLPolicy(owned[0], desiredDNSBLPolicy(cfg)) {
		return fmt.Errorf("content-filter DNSBL policy is drifted")
	}
	return nil
}
