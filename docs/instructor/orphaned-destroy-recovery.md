# Orphaned destroy recovery

When a pod is stuck in `destroy_failed` with `manual_cleanup_required` because the durable portgroup receipt is missing or invalid, and an operator has independently confirmed there is no live vCenter, OPNsense, Kea, or firewall residue, an admin may finalize the orphaned database state with the narrow recovery endpoint:

`POST /api/v1/admin/pods/{podID}/finalize-orphaned-destroy`

Body:

```json
{
  "vlan_id": 102,
  "subnet": "10.100.2.0/24",
  "confirmation_token": "operator-confirmed-zero-residue"
}
```

The endpoint is admin-only, fail-closed, and does not call external infrastructure. It requires the pod to still be `destroy_failed`, the authoritative destroy job to remain in `manual_cleanup_required`, and the attestation to match the pod’s recorded VLAN and subnet exactly.

Use this only after out-of-band inspection has already proven the pod is a pure orphaned database record. It is not a general destroy-failure bypass.
