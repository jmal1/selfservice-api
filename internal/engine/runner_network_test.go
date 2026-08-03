package engine

import (
	"context"
	"encoding/json"
	"strconv"
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
	if !ok || ipam["type"] != "dhcp" {
		t.Errorf("NAD ipam = %v, want type dhcp", nadCfg["ipam"])
	}
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
	equivalent := `{ "ipam": {"type":"dhcp"}, "master":"` + cfg.TrunkNIC + `", "vlanId":119, "type":"vlan", "cniVersion":"0.3.1" }`
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
