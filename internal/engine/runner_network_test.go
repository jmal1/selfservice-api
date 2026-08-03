package engine

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func nadGVRForTest() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "k8s.cni.cncf.io",
		Version:  "v1",
		Resource: "network-attachment-definitions",
	}
}

func newNADTestClient(t *testing.T, cfg K8sConfig) (*K8sClient, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	gvr := nadGVRForTest()
	scheme.AddKnownTypeWithName(gvr.GroupVersion().WithKind("NetworkAttachmentDefinition"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvr.GroupVersion().WithKind("NetworkAttachmentDefinitionList"), &unstructured.UnstructuredList{})

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{gvr: "NetworkAttachmentDefinitionList"},
	)
	return NewK8sClientFromClients(fake.NewSimpleClientset(), dynClient, cfg, testLogger()), dynClient
}

// readNADConfig returns the parsed CNI config embedded in a created NAD.
func readNADConfig(t *testing.T, dynClient *dynamicfake.FakeDynamicClient, ns, name string) map[string]any {
	t.Helper()
	obj, err := dynClient.Resource(nadGVRForTest()).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NAD %s: %v", name, err)
	}
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("NAD has no spec: %#v", obj.Object)
	}
	raw, ok := spec["config"].(string)
	if !ok {
		t.Fatalf("NAD spec.config is not a string: %#v", spec)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("NAD config is not valid JSON: %v\n%s", err, raw)
	}
	return cfg
}

// TestEnsureNAD_UsesVLANPluginNotMacvlan guards the defect that put the
// assessment runner on the wrong network entirely.
//
// The NAD used to be {"type":"macvlan","master":<nic>,"vlan":<tag>,...}. The
// macvlan CNI plugin has no `vlan` option; CNI plugins ignore unknown fields,
// so the tag was silently dropped and the runner attached UNTAGGED to the
// trunk NIC. It then DHCP'd on the trunk's native VLAN and received a home-LAN
// address instead of a pod-VLAN one — simultaneously a network isolation
// failure and the reason the runner could never reach its target VM.
//
// Nothing detected it: the plugin returned success, Multus reported
// "AddedInterface ... from pod-vlan-119", and the pod genuinely had a second
// interface with a valid DHCP lease. Only the address range revealed it.
func TestEnsureNAD_UsesVLANPluginNotMacvlan(t *testing.T) {
	cfg := testK8sConfig()
	k8s, dynClient := newNADTestClient(t, cfg)

	if err := k8s.ensureNAD(context.Background(), "pod-vlan-119", 119); err != nil {
		t.Fatalf("ensureNAD: %v", err)
	}

	nadCfg := readNADConfig(t, dynClient, cfg.Namespace, "pod-vlan-119")

	if got := nadCfg["type"]; got != "vlan" {
		t.Errorf("NAD type = %v, want \"vlan\". The macvlan plugin silently ignores "+
			"the VLAN tag and attaches the runner to the trunk's native VLAN.", got)
	}
	if _, hasMacvlanField := nadCfg["vlan"]; hasMacvlanField {
		t.Error(`NAD still carries the macvlan-style "vlan" key; the vlan plugin ` +
			`expects "vlanId" and would silently ignore "vlan"`)
	}
	// JSON numbers decode as float64.
	if got, ok := nadCfg["vlanId"].(float64); !ok || int(got) != 119 {
		t.Errorf("NAD vlanId = %v (%T), want 119", nadCfg["vlanId"], nadCfg["vlanId"])
	}
	if got := nadCfg["master"]; got != cfg.TrunkNIC {
		t.Errorf("NAD master = %v, want %q", got, cfg.TrunkNIC)
	}
	ipam, ok := nadCfg["ipam"].(map[string]any)
	if !ok || ipam["type"] != "host-local" {
		t.Errorf("NAD ipam = %v, want type host-local", nadCfg["ipam"])
	}
}

// TestNADConfig_InstallsNoDefaultRoute guards the defect that let the runner
// execute a whole assessment and then silently fail to deliver any result.
//
// With ipam=dhcp, OPNsense's router option made the CNI dhcp plugin return a
// 0.0.0.0/0 route, so the pod ended up with TWO default routes and the pod
// VLAN's won. Nothing routes the Service CIDR (10.43.0.0/16) explicitly, so
// cluster DNS and the engine ClusterIP both followed that default out of net1
// and died. The Job still exited 0.
//
// The fix is the ABSENCE of routes, which is easy to reintroduce by "helpfully"
// adding a gateway, so assert the absence directly.
func TestNADConfig_InstallsNoDefaultRoute(t *testing.T) {
	cfg := testK8sConfig()
	k8s, dynClient := newNADTestClient(t, cfg)

	if err := k8s.ensureNAD(context.Background(), "pod-vlan-119", 119); err != nil {
		t.Fatalf("ensureNAD: %v", err)
	}
	nadCfg := readNADConfig(t, dynClient, cfg.Namespace, "pod-vlan-119")

	ipam, ok := nadCfg["ipam"].(map[string]any)
	if !ok {
		t.Fatalf("NAD has no ipam block: %v", nadCfg)
	}
	if ipam["type"] == "dhcp" {
		t.Fatal("ipam is back to dhcp: OPNsense answers with a router option, so " +
			"the pod gets a second default route via the pod VLAN and can no longer " +
			"reach cluster DNS or the engine's ClusterIP")
	}
	if _, has := ipam["routes"]; has {
		t.Errorf(`ipam declares "routes" (%v); any route here risks re-adding a `+
			`default that shadows Flannel's and breaks the runner's callback`, ipam["routes"])
	}
	if _, has := ipam["gateway"]; has {
		t.Error(`ipam declares "gateway"; the runner only ever talks to hosts on ` +
			`its own /24, and a gateway invites a default route back in`)
	}
	// The default route must remain Flannel's, which means the NAD must not
	// describe one anywhere in the config, at any nesting level.
	raw, _ := json.Marshal(nadCfg)
	if strings.Contains(string(raw), "0.0.0.0/0") {
		t.Errorf("NAD config mentions 0.0.0.0/0: %s", raw)
	}
}

// TestNADConfig_SubnetMatchesVLANConvention pins the tag->subnet mapping to the
// one vlan_pool.subnet and provisioner.createPod use. If these ever diverge the
// runner gets an address on a network its target VM is not on, and every action
// fails with a connectivity error that looks like a broken target.
func TestNADConfig_SubnetMatchesVLANConvention(t *testing.T) {
	cfg := testK8sConfig()
	k8s, _ := newNADTestClient(t, cfg)

	for _, tc := range []struct {
		tag        int
		wantSubnet string
	}{
		{105, "10.100.5.0/24"},
		{119, "10.100.19.0/24"},
		{250, "10.100.150.0/24"},
	} {
		cfgJSON, err := k8s.nadConfigJSON(tc.tag)
		if err != nil {
			t.Fatalf("nadConfigJSON(%d): %v", tc.tag, err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(cfgJSON), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		ipam := m["ipam"].(map[string]any)
		r := ipam["ranges"].([]any)[0].([]any)[0].(map[string]any)
		if r["subnet"] != tc.wantSubnet {
			t.Errorf("vlan %d subnet = %v, want %s", tc.tag, r["subnet"], tc.wantSubnet)
		}
	}
}

// TestNADConfig_RangeAvoidsDHCPPool guards the collision the runner would
// otherwise have with a student VM. OPNsense serves .10-.250 on every pod VLAN
// (provisioner.createPod), so the runner's range must start above .250.
func TestNADConfig_RangeAvoidsDHCPPool(t *testing.T) {
	cfg := testK8sConfig()
	k8s, _ := newNADTestClient(t, cfg)

	cfgJSON, err := k8s.nadConfigJSON(119)
	if err != nil {
		t.Fatalf("nadConfigJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(cfgJSON), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r := m["ipam"].(map[string]any)["ranges"].([]any)[0].([]any)[0].(map[string]any)

	const opnsenseDHCPPoolEnd = 250 // provisioner.createPod: "10.100.%d.10-10.100.%d.250"
	start, end := r["rangeStart"].(string), r["rangeEnd"].(string)
	startHost := hostOctet(t, start)
	endHost := hostOctet(t, end)

	if startHost <= opnsenseDHCPPoolEnd {
		t.Errorf("rangeStart %s is inside the OPNsense DHCP pool (.10-.%d); a runner "+
			"could be handed the same address as a student VM", start, opnsenseDHCPPoolEnd)
	}
	if endHost > 254 {
		t.Errorf("rangeEnd %s is the broadcast address or beyond", end)
	}
	if startHost > endHost {
		t.Errorf("range %s-%s is inverted", start, end)
	}
}

// TestNADConfig_RejectsOutOfRangeVLAN ensures a bad tag fails loudly rather
// than producing a config for a subnet like 10.100.-5.0/24.
func TestNADConfig_RejectsOutOfRangeVLAN(t *testing.T) {
	cfg := testK8sConfig()
	k8s, _ := newNADTestClient(t, cfg)

	for _, tag := range []int{0, 100, 355, -1} {
		if _, err := k8s.nadConfigJSON(tag); err == nil {
			t.Errorf("nadConfigJSON(%d) = nil error, want rejection", tag)
		}
	}
}

func hostOctet(t *testing.T, addr string) int {
	t.Helper()
	parts := strings.Split(addr, ".")
	if len(parts) != 4 {
		t.Fatalf("malformed address %q", addr)
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil {
		t.Fatalf("malformed address %q: %v", addr, err)
	}
	return n
}

// TestEnsureNAD_TagIsPlumbedThrough ensures the VLAN actually varies with the
// pod's tag rather than being pinned to a constant.
func TestEnsureNAD_TagIsPlumbedThrough(t *testing.T) {
	cfg := testK8sConfig()
	k8s, dynClient := newNADTestClient(t, cfg)

	for _, tag := range []int{105, 119, 250} {
		name := "pod-vlan-" + strconv.Itoa(tag)
		if err := k8s.ensureNAD(context.Background(), name, tag); err != nil {
			t.Fatalf("ensureNAD(%d): %v", tag, err)
		}
		cfgMap := readNADConfig(t, dynClient, cfg.Namespace, name)
		got, ok := cfgMap["vlanId"].(float64)
		if !ok || int(got) != tag {
			t.Errorf("NAD %s vlanId = %v, want %d", name, cfgMap["vlanId"], tag)
		}
	}
}

// TestEnsureNAD_IsIdempotent covers re-running against an existing NAD, which
// happens on every subsequent run for the same pod.
func TestEnsureNAD_IsIdempotent(t *testing.T) {
	cfg := testK8sConfig()
	k8s, dynClient := newNADTestClient(t, cfg)

	if err := k8s.ensureNAD(context.Background(), "pod-vlan-119", 119); err != nil {
		t.Fatalf("first ensureNAD: %v", err)
	}
	if err := k8s.ensureNAD(context.Background(), "pod-vlan-119", 119); err != nil {
		t.Fatalf("second ensureNAD should be a no-op, got: %v", err)
	}

	list, err := dynClient.Resource(nadGVRForTest()).Namespace(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list NADs: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("got %d NADs, want exactly 1 after two ensureNAD calls", len(list.Items))
	}
}

// TestEnsureNAD_ReconcilesStaleMacvlanConfig is the guard for a trap that
// would have made the macvlan->vlan fix a silent no-op in production.
//
// ensureNAD originally returned as soon as a NAD of the right name existed, so
// any VLAN that had already been used kept its old CNI config forever. At the
// time of the fix, pod-vlan-119 was live in the cluster carrying the broken
// untagged macvlan config -- deploying the corrected code would have changed
// nothing for it, and the runner would have kept landing on the home LAN while
// every code-level check said the bug was fixed.
func TestEnsureNAD_ReconcilesStaleMacvlanConfig(t *testing.T) {
	cfg := testK8sConfig()
	k8s, dynClient := newNADTestClient(t, cfg)

	// The exact config observed in production before the fix.
	stale := `{"cniVersion":"0.3.1","ipam":{"type":"dhcp"},"master":"ens224","mode":"bridge","type":"macvlan","vlan":119}`
	seed := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]any{
				"name":      "pod-vlan-119",
				"namespace": cfg.Namespace,
			},
			"spec": map[string]any{"config": stale},
		},
	}
	if _, err := dynClient.Resource(nadGVRForTest()).Namespace(cfg.Namespace).
		Create(context.Background(), seed, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed stale NAD: %v", err)
	}

	if err := k8s.ensureNAD(context.Background(), "pod-vlan-119", 119); err != nil {
		t.Fatalf("ensureNAD: %v", err)
	}

	got := readNADConfig(t, dynClient, cfg.Namespace, "pod-vlan-119")
	if got["type"] != "vlan" {
		t.Errorf("stale NAD was not reconciled: type = %v, want \"vlan\". "+
			"An existing NAD must be corrected, not skipped.", got["type"])
	}
	if _, stillHasMacvlanKey := got["vlan"]; stillHasMacvlanKey {
		t.Error(`stale NAD still carries the macvlan-style "vlan" key`)
	}
	if v, ok := got["vlanId"].(float64); !ok || int(v) != 119 {
		t.Errorf("reconciled NAD vlanId = %v, want 119", got["vlanId"])
	}
}

// TestEnsureNAD_NoPointlessUpdateWhenAlreadyCorrect ensures reconciliation does
// not rewrite the object on every single run. Key ordering must not count as
// drift, or every run would issue an Update against the API server.
func TestEnsureNAD_NoPointlessUpdateWhenAlreadyCorrect(t *testing.T) {
	cfg := testK8sConfig()
	k8s, dynClient := newNADTestClient(t, cfg)

	// Semantically identical to what nadConfigJSON emits, but key order and
	// spacing differ.
	equivalent := `{ "master":"` + cfg.TrunkNIC + `", "type":"vlan", "vlanId":119, "cniVersion":"0.3.1",
	  "ipam": { "ranges": [ [ { "rangeEnd":"10.100.19.254", "subnet":"10.100.19.0/24", "rangeStart":"10.100.19.251" } ] ], "type":"host-local" } }`
	seed := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata":   map[string]any{"name": "pod-vlan-119", "namespace": cfg.Namespace},
			"spec":       map[string]any{"config": equivalent},
		},
	}
	if _, err := dynClient.Resource(nadGVRForTest()).Namespace(cfg.Namespace).
		Create(context.Background(), seed, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed NAD: %v", err)
	}

	dynClient.ClearActions()
	if err := k8s.ensureNAD(context.Background(), "pod-vlan-119", 119); err != nil {
		t.Fatalf("ensureNAD: %v", err)
	}
	for _, a := range dynClient.Actions() {
		if a.GetVerb() == "update" {
			t.Error("ensureNAD issued an Update for a config that only differs in key order/whitespace")
		}
	}
}
