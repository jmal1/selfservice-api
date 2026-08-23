package vcenter

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

type portGroupMutationRecorder struct {
	next               soap.RoundTripper
	failAddReceiver    string
	failRemoveReceiver string

	mu         sync.Mutex
	operations []string
}

func TestLegacyPortGroupReceiptIsDiagnosedBeforeMutation(t *testing.T) {
	_, err := (&Client{}).validatePortGroupReceipt(
		context.Background(),
		PortGroupReceipt{Name: "Pod-VLAN123"},
	)
	if !errors.Is(err, ErrLegacyPortGroupReceipt) {
		t.Fatalf("legacy receipt error = %v, want ErrLegacyPortGroupReceipt", err)
	}
}

func (r *portGroupMutationRecorder) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	switch body := req.(type) {
	case *methods.AddPortGroupBody:
		if body.Req != nil {
			receiver := body.Req.This.Value
			r.record("add:" + receiver)
			if receiver == r.failAddReceiver {
				return errors.New("simulated AddPortGroup failure")
			}
		}
	case *methods.RemovePortGroupBody:
		if body.Req != nil {
			receiver := body.Req.This.Value
			r.record("remove:" + receiver)
			if receiver == r.failRemoveReceiver {
				return errors.New("simulated RemovePortGroup failure")
			}
		}
	}
	return r.next.RoundTrip(ctx, req, res)
}

func (r *portGroupMutationRecorder) record(operation string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.operations = append(r.operations, operation)
}

func (r *portGroupMutationRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.operations...)
}

func TestPortGroupPartialCreateCompensatesOnlyCreatedHosts(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		host1, host2 := twoHostsInResourcePool(t, ctx, c, simResourcePool)
		setAllowedHosts(c, host1, host2)
		ns1 := networkSystemMoref(t, ctx, c, host1)
		ns2 := networkSystemMoref(t, ctx, c, host2)

		receipt, err := c.PlanPortGroupMutation(ctx, "Pod-VLAN401", 401)
		if err != nil {
			t.Fatal(err)
		}

		original := c.client.RoundTripper
		recorder := &portGroupMutationRecorder{
			next:            original,
			failAddReceiver: ns2,
		}
		c.client.RoundTripper = recorder
		defer func() { c.client.RoundTripper = original }()

		if err := c.ApplyPortGroupMutation(ctx, receipt); err == nil {
			t.Fatal("ApplyPortGroupMutation succeeded after the second host AddPortGroup failed")
		}
		wantOperations := []string{"add:" + ns1, "add:" + ns2, "remove:" + ns1}
		if got := recorder.snapshot(); !reflect.DeepEqual(got, wantOperations) {
			t.Fatalf("portgroup operations = %v, want %v", got, wantOperations)
		}
		for _, host := range []HostIdentity{host1, host2} {
			if _, found, err := c.findPortGroupOnHost(ctx, host, receipt.Name); err != nil {
				t.Fatal(err)
			} else if found {
				t.Fatalf("partial portgroup create leaked onto %s", host.Name)
			}
		}
	})
}

func TestPortGroupMutationTouchesOnlySelectedTargetHosts(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		host1, host2 := twoHostsInResourcePool(t, ctx, c, simResourcePool)
		setAllowedHosts(c, host1, host2)
		ns2 := networkSystemMoref(t, ctx, c, host2)

		receipt, err := c.PlanPortGroupMutationForHosts(
			ctx,
			"Pod-VLAN405",
			405,
			[]string{host2.MoRef},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(receipt.Hosts) != 1 || receipt.Hosts[0].HostMoRef != host2.MoRef {
			t.Fatalf("receipt hosts = %+v, want only %s", receipt.Hosts, host2.MoRef)
		}

		original := c.client.RoundTripper
		recorder := &portGroupMutationRecorder{next: original}
		c.client.RoundTripper = recorder
		defer func() { c.client.RoundTripper = original }()

		if err := c.ApplyPortGroupMutation(ctx, receipt); err != nil {
			t.Fatal(err)
		}
		if got := recorder.snapshot(); !reflect.DeepEqual(got, []string{"add:" + ns2}) {
			t.Fatalf("portgroup operations = %v, want target host only", got)
		}
		if _, found, err := c.findPortGroupOnHost(ctx, host1, receipt.Name); err != nil {
			t.Fatal(err)
		} else if found {
			t.Fatalf("portgroup %s was created on unselected host %s", receipt.Name, host1.Name)
		}
		if err := c.DeletePortGroupMutation(ctx, receipt); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPortGroupReceiptPreservesPreexistingPerHostState(t *testing.T) {
	host1 := PortGroupHostReceipt{HostName: "esxi1", HostMoRef: "host-1", ComputeMoRef: "domain-c1", Preexisting: true}
	host2 := PortGroupHostReceipt{HostName: "esxi2", HostMoRef: "host-2", ComputeMoRef: "domain-c1"}
	receipt := PortGroupReceipt{Name: "Pod-VLAN402", VLANID: 402, Hosts: []PortGroupHostReceipt{host1, host2}}
	present := map[string]bool{"host-1": true, "host-2": false}
	var operations []string
	add := func(_ context.Context, host PortGroupHostReceipt, _ string, _ int) error {
		operations = append(operations, "add:"+host.HostMoRef)
		return nil
	}
	remove := func(_ context.Context, host PortGroupHostReceipt, _ string) error {
		operations = append(operations, "remove:"+host.HostMoRef)
		return nil
	}

	if err := applyPortGroupReceipt(context.Background(), receipt, present, add, remove); err != nil {
		t.Fatal(err)
	}
	if err := deletePortGroupReceipt(context.Background(), receipt, remove); err != nil {
		t.Fatal(err)
	}
	wantOperations := []string{"add:host-2", "remove:host-2"}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("clone-failure receipt cleanup operations = %v, want %v", operations, wantOperations)
	}
}

func TestPortGroupDeleteRejectsHistoricalDisallowedHostBeforeMutation(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		host1, host2 := twoHostsInResourcePool(t, ctx, c, simResourcePool)
		setAllowedHosts(c, host1)

		original := c.client.RoundTripper
		recorder := &portGroupMutationRecorder{next: original}
		c.client.RoundTripper = recorder
		defer func() { c.client.RoundTripper = original }()

		err := c.DeletePortGroupMutation(ctx, PortGroupReceipt{
			Name:   "Pod-VLAN403",
			VLANID: 403,
			Hosts: []PortGroupHostReceipt{{
				HostName:     host2.Name,
				HostMoRef:    host2.MoRef,
				ComputeMoRef: host2.ComputeMoRef,
			}},
		})
		if !errors.Is(err, ErrHostNotAllowed) {
			t.Fatalf("historical disallowed receipt error = %v, want ErrHostNotAllowed", err)
		}
		if operations := recorder.snapshot(); len(operations) != 0 {
			t.Fatalf("disallowed receipt attempted switch mutations: %v", operations)
		}
	})
}

func TestPortGroupDeleteSurfacesFailureForRetry(t *testing.T) {
	receipt := PortGroupReceipt{
		Name:   "Pod-VLAN404",
		VLANID: 404,
		Hosts: []PortGroupHostReceipt{{
			HostName:     "esxi1",
			HostMoRef:    "host-1",
			ComputeMoRef: "domain-c1",
		}},
	}
	err := deletePortGroupReceipt(
		context.Background(),
		receipt,
		func(context.Context, PortGroupHostReceipt, string) error {
			return errors.New("simulated RemovePortGroup failure")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "simulated RemovePortGroup failure") {
		t.Fatalf("delete error = %v, want retryable RemovePortGroup failure", err)
	}
}

func twoHostsInResourcePool(
	t *testing.T,
	ctx context.Context,
	c *Client,
	poolPath string,
) (HostIdentity, HostIdentity) {
	t.Helper()
	pool, err := c.finder.ResourcePool(ctx, poolPath)
	if err != nil {
		t.Fatal(err)
	}
	var poolProps mo.ResourcePool
	if err := pool.Properties(ctx, pool.Reference(), []string{"owner"}, &poolProps); err != nil {
		t.Fatal(err)
	}
	hosts, err := c.resolvedHosts()
	if err != nil {
		t.Fatal(err)
	}
	var matches []HostIdentity
	for _, host := range hosts {
		if host.ComputeMoRef == poolProps.Owner.Value {
			matches = append(matches, host)
		}
	}
	if len(matches) < 2 {
		t.Fatalf("resource pool %s has %d simulator hosts, want at least two", poolPath, len(matches))
	}
	return matches[0], matches[1]
}

func setAllowedHosts(c *Client, hosts ...HostIdentity) {
	c.hostMu.Lock()
	defer c.hostMu.Unlock()
	c.allowedHosts = append([]HostIdentity(nil), hosts...)
	c.config.Hosts = make([]string, 0, len(hosts))
	for _, host := range hosts {
		c.config.Hosts = append(c.config.Hosts, host.Name)
	}
}

func networkSystemMoref(t *testing.T, ctx context.Context, c *Client, identity HostIdentity) string {
	t.Helper()
	host := object.NewHostSystem(c.client.Client, types.ManagedObjectReference{
		Type:  "HostSystem",
		Value: identity.MoRef,
	})
	networkSystem, err := host.ConfigManager().NetworkSystem(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if networkSystem == nil {
		t.Fatalf("host %s has no network system", identity.Name)
	}
	return networkSystem.Reference().Value
}
