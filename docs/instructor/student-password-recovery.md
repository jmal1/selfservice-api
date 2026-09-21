# Student password recovery (Authentik)

Students authenticate through Authentik, not through Crucible. When a student
forgets their password or is locked out of SSO, **do not invent a Crucible-side
reset**. Use the limited Authentik helpdesk workflow below.

> [!tip] Prefer recovery links
> Create a recovery link and send it to the student through a private channel.
> Prefer that over setting a password yourself. The student then chooses their
> own credential in Authentik's recovery flow.

## Who you can help

Only accounts under the student path tree (for example `users/students/2027`,
or the current cohort folder your admin documented). You cannot reset other
instructors, admins, or service accounts — Authentik will deny those actions.

## Steps

1. Sign in to Authentik Admin: `https://authentik.jmal.io` (same MFA you use for Crucible).
2. Open **Directory** → **Users**.
3. Open the cohort path (for example **`users/students/2027`**).
4. Select the student.
5. Prefer **Create recovery link**, copy the URL, and send it privately.
6. If you must use **Set password**, give them a strong temporary password in person or via an encrypted channel, and tell them to change it after sign-in.

## What students are told

The Student Guide says: forgot password → talk to your instructor. Point them
at Crucible **Guide** → Troubleshooting if they ask where that is written.

## If Authentik says permission denied

Your account is missing the helpdesk object permission on that user, or the
user is outside the student path. Ask a platform admin to run the helpdesk
reconciler (see the vault page **Instructor Authentik Helpdesk**).

## Related

- Platform ops: vault `current/14-Instructor-Authentik-Helpdesk.md`
- First-login force-change behavior: vault `current/10-First-Login-Password-Change.md`
