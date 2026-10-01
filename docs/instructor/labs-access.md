# Isolated labs and Single VM access

Students do not get isolated labs or blueprints unless an instructor grants
them. The grant is a column on the user, `labs_enabled`, edited from the
admin Users page. It is not an Authentik group. Instructors and admins
always have labs.

`labs_enabled` defaults to false. The API only enforces it when
`LABS_REQUIRE_GRANT` is true. The Helm value `admission.labsRequireGrant`
defaults to false, so merging this change does not lock students out. The
cutover deploy is what turns the gate on.

`GET /api/v1/auth/me` returns the student's `labs_enabled` on the user and
`labs_require_grant` for the process. The UI hides Labs and Deploy when the
grant is required and the student does not have it.

A template can be marked **Single VM only** from its settings
(`templates.single_vm_only`). While that flag is true:

- saving a blueprint that uses the template returns 409 and names the template
- creating an isolated pod, deploying a blueprint, or adding a VM from that
  template returns 409
- turning the flag on while an active blueprint still uses the template
  returns 409 and names those blueprints

Login does not overwrite `labs_enabled` or `max_single_vms`. A student's
Single VM cap is `max_single_vms` (default 1, maximum 3), also edited on the
Users page. Instructors and admins cannot be given this grant; they already
have labs.

A Single VM is a pod with `network_mode=shared` on one of nine stripes,
VLAN tags 347–355, in `10.110.0.0/16`. Those tags are not in the isolated
pool. The nine networks are built by one instructor job,
`POST /api/v1/admin/shared-networks/provision`, before any student deploy.
`POST /api/v1/single-vms` only places a VM on a stripe that is already
active and has fewer than 16 VMs. It does not create a VLAN. If the stripes
are not ready, or all 144 slots are full, the request returns 409.
A shared assessment reserves one of the fourteen runner addresses on that
stripe (`.2` through `.15`) and attaches the runner with a static address
on the stripe's `/26`. It does not use the isolated `10.100.0.0/16`
host-local range. If every runner address on the stripe is in use, the
request returns 503 and does not start a runner. A Single VM does not count
toward `max_pods`.
Lab VMs and Single VMs both suspend after the same idle window, which is
2 hours.
