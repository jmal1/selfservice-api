# Work-lease external mutation contract
#
# Named reconciler leases (internal/worklease) provide at-least-once exclusivity,
# not a distributed mutex. Operators and future changes must keep each surface
# safe under reclaim:
#
# | Surface              | Required safety                                      |
# |----------------------|------------------------------------------------------|
# | OPNsense network     | Desired-state reconcile; no non-idempotent appends   |
# | vCenter orphan destroy | Re-validate identity/ownership immediately before destroy |
# | Template orphan      | Same as above for Templates-folder disposables       |
# | Idle suspend         | Recheck activity + lifecycle immediately before suspend; IdleEval lease holder both evaluates and pushes shared suspend gauges |
# | Template health clone| Deterministic name / adopt existing healthcheck VM   |
# | Pod / template clone | Job claim_token CAS; adopt-before-create where possible |
#
# On lease loss: stop before the next irreversible external step; do not write
# terminal DB state with a stale claim token. Postgres outage ⇒ fail closed on
# new mutations (same floor as the old advisory-lock leader).
