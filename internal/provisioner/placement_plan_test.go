package provisioner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/rollback"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

func TestSelectedPlacementHostsReturnsSortedUnion(t *testing.T) {
	placements := []models.VMPlacement{
		{HostMoref: "host-3"},
		{HostMoref: "host-1"},
		{HostMoref: "host-3"},
		{HostMoref: "host-2"},
	}
	got := selectedPlacementHosts(placements)
	want := []string{"host-1", "host-2", "host-3"}
	if len(got) != len(want) {
		t.Fatalf("selected hosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selected hosts = %v, want %v", got, want)
		}
	}
}

func TestExistingPortGroupReceiptRequiresSelectedHostUnion(t *testing.T) {
	jobID := uuid.New()
	engine := rollback.New(jobID, nil, nil)
	receipt := vcenter.PortGroupReceipt{
		Name:   "Pod-VLAN500",
		VLANID: 500,
		Hosts: []vcenter.PortGroupHostReceipt{{
			HostName:     "esxi1",
			HostMoRef:    "host-1",
			ComputeMoRef: "domain-c1",
		}},
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	engine.LoadSteps([]rollback.Step{{
		Name: "portgroup_create",
		Data: data,
	}})

	_, err = existingPortGroupReceipt(engine, receipt.Name, receipt.VLANID, []string{"host-1", "host-2"})
	if err == nil || !strings.Contains(err.Error(), "host-2") {
		t.Fatalf("missing host coverage error = %v", err)
	}
}

func TestExistingPortGroupReceiptUsesFirstAuthoritativeReceipt(t *testing.T) {
	engine := rollback.New(uuid.New(), nil, nil)
	first := vcenter.PortGroupReceipt{
		Name:   "Pod-VLAN500",
		VLANID: 500,
		Hosts: []vcenter.PortGroupHostReceipt{{
			HostName:     "esxi1",
			HostMoRef:    "host-1",
			ComputeMoRef: "domain-c1",
		}},
	}
	retry := vcenter.PortGroupReceipt{
		Name:   first.Name,
		VLANID: first.VLANID,
		Hosts: []vcenter.PortGroupHostReceipt{{
			HostName:     "esxi2",
			HostMoRef:    "host-2",
			ComputeMoRef: "domain-c1",
			Preexisting:  true,
		}},
	}
	firstData, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	retryData, err := json.Marshal(retry)
	if err != nil {
		t.Fatal(err)
	}
	engine.LoadSteps([]rollback.Step{
		{Name: "portgroup_create", Data: firstData},
		{Name: "portgroup_create", Data: retryData},
	})

	got, err := existingPortGroupReceipt(engine, first.Name, first.VLANID, []string{"host-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Hosts) != 1 || got.Hosts[0].HostMoRef != "host-1" {
		t.Fatalf("selected receipt = %+v, want first authoritative receipt", got)
	}
}
