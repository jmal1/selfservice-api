# Building Playlists

A **playlist** is an ordered list of workflows presented to the student
as a single graded lab. Most of your authoring time goes into
[workflows](workflows.md); playlists are usually a five-minute job to
assemble.

---

## Why playlists exist

Workflows grade one thing. Real labs need a dozen things. Without
playlists, instructors would have to ask students to click "Run" a
dozen times.

A playlist:

- Groups related workflows into a single click for the student
- Defines the order in which they run
- Surfaces a single overall pass/fail summary
- Becomes the unit you publish to a class section

---

## Shape of a playlist

Playlists are created via `POST /api/v1/admin/playlists`. The minimal
payload is just `name`, `slug`, and a list of workflow slugs in order:

```json
{
  "name": "Week 3 — SSH Hardening",
  "slug": "ssh-hardening-week3",
  "description": "Validates SSH hardening: key auth, no root login, fail2ban, firewall.",
  "category": "hardening",
  "blueprint_slug": "ubuntu-target-pair",
  "workflows": [
    {"slug": "ssh-running", "order": 1, "required": true},
    {"slug": "ssh-key-auth-only", "order": 2, "required": true},
    {"slug": "ssh-no-root-login", "order": 3, "required": true},
    {"slug": "fail2ban-enabled", "order": 4, "required": false},
    {"slug": "port-23-blocked", "order": 5, "required": true}
  ],
  "visible_to_students": true,
  "status": "draft"
}
```

### Field reference

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Human-readable lab title. |
| `slug` | yes | `[a-z0-9-]+`, globally unique. |
| `description` | no | Markdown. Shown to students on the playlist page. |
| `category` | no | Freeform tag. |
| `blueprint_slug` | yes | Which pod blueprint this playlist is designed for. The student must have an active pod from this blueprint to run it. |
| `workflows[]` | yes | Ordered list. Each entry must reference an `active` workflow. |
| `workflows[].slug` | yes | The workflow's slug. |
| `workflows[].order` | yes | Integer; lower runs first. Reuse not allowed. |
| `workflows[].required` | no | If `false`, a failure won't fail the overall playlist. Use sparingly — students learn ordering matters. |
| `visible_to_students` | no | If `false`, only instructors can see + run it. |
| `status` | no | `draft` → `active`. Only `active` playlists appear in the student picker. |

---

## Ordering — what to put first

Order matters because workflows can fail and abort. Practical rules:

1. **Setup first.** Workflows that prep the environment (e.g.
   "verify pod is reachable") go before grading workflows.
2. **Cheapest first.** A 2-second connectivity check should come before
   a 60-second port scan. Fast failures = fast feedback.
3. **Independent assertions.** If two workflows test unrelated things,
   either order is fine — but pick one and stay consistent across the
   class so students aren't confused.
4. **Optional last.** Anything `required: false` should come at the end
   so the required core can finish quickly.

> [!tip]
> Think of a playlist as a checklist with a passing grade. Order the
> checks the way you'd debug them yourself.

---

## What students see

When a student clicks **Run** on a playlist:

1. They see a progress bar with each workflow's status
   (`pending` → `running` → `pass` / `fail`).
2. For each `visible_to_students: true` workflow, they see the name,
   any `STUDENT_MSG:` lines, and a green/red status.
3. For each `visible_to_students: false` workflow, they see a generic
   "Instructor check" entry with no detail — useful for sanity checks
   you don't want students to game.
4. The overall playlist result is **pass only if every `required: true`
   workflow passes**.

> [!important]
> A failed required workflow short-circuits the rest of the playlist
> by default — see the engine's `stop_on_failure` config. If you want
> "always run all checks even if one fails", set
> `stop_on_failure: false` on the playlist.

---

## Lifecycle

Playlists have a simpler lifecycle than workflows:

```
draft  →  active
```

That's it. There's no `pending_review` because playlists are *composed*
of already-approved workflows — the review happens at the workflow
level.

> [!warning]
> Promoting a playlist to `active` while one of its referenced
> workflows is still in `draft` will be rejected by the API. Get the
> workflows to `active` first, then build the playlist.

---

## Editing a published playlist

Same rule as workflows: edits to an `active` playlist create a **new
revision**. In-flight student runs continue against the version they
launched with. The "head" revision is what new student clicks will use.

> [!tip]
> If you've published a playlist and notice a bug mid-class, fix it
> immediately — already-running students stay on the buggy revision,
> but new clicks pick up the fix. No need to disrupt anyone.

---

## Common patterns

### Smoke-test playlist for blueprint validation

A short playlist with no `STUDENT_MSG` output that verifies a blueprint
is deploying correctly. Set `visible_to_students: false` and run it
yourself after pushing template changes.

### Per-week playlists for a course

One playlist per week, all keyed off the same blueprint. Easy to compare
student progress over time.

### Progressive disclosure

Split a complex lab into 3-4 sequential playlists rather than one
mega-playlist. Students see one playable item at a time; instructors
get clearer per-topic pass rates.

---

## Assigning playlists to blueprints

Playlists are assigned at the **template** level, not the blueprint level.
When you create or edit a template, you specify which playlists should run
on each VM in that template. This is called the **template default**.

### Template defaults

A template can have one or more playlists. When a blueprint uses that template,
all VMs from that template inherit its default playlists automatically.

**Example**: If you create a template called "Ubuntu-22.04-target" and assign
the playlist "SSH Hardening" to it, then every blueprint that includes an
"Ubuntu-22.04-target" VM will run "SSH Hardening" on that slot.

### Blueprint-level overrides

Sometimes you want a specific VM slot in a blueprint to run *different*
playlists than the template default. You can override the template's defaults
at the blueprint level.

**Example**: You have a blueprint called "ubuntu-duo" with two VMs, both from
the "Ubuntu-22.04-target" template. Normally both would run "SSH Hardening".
But you want the second VM (the "victim") to run a different playlist called
"SSH Misconfiguration". You can override the second VM slot to run
"SSH Misconfiguration" instead, while the first VM still runs "SSH Hardening".

### Seeing which playlists will actually run

When editing or reviewing a blueprint, you can view the **resolved** playlists
for each VM slot — the list that accounts for both template defaults and
any blueprint-level overrides.

In the blueprint editor, click the **Playlists** button on any VM slot to see:
- The name of each assigned playlist
- Whether it came from the template default (green badge) or a blueprint override (blue badge)

This prevents the silent surprise where you edit a template's default playlists
but don't realize certain blueprints won't pick up your change (because they
have overrides).

### Reverting an override

If you've overridden a VM slot's playlists but want it to go back to the
template defaults, click **Revert to Template** in the playlists view.
This removes the override and the VM will immediately start inheriting
the template's defaults again.

---

## See also

- [Building Workflows](workflows.md) — what playlists are composed of
- [Overview](overview.md) — how playlists fit in the bigger picture
- [Troubleshooting](troubleshooting.md) — when a playlist won't run
