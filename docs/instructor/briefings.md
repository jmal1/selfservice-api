# Lab Briefings

A **briefing** is a Markdown write-up for a lab: the scenario, the objectives, the rules of engagement, and any commands students should start with. You attach one briefing to a **template** (including a Single VM template) or to a **blueprint**. Students see it only **after they provision** a pod that uses it.

> [!note]
> This page describes the API that stores briefings. The **Briefing** tab in the template and blueprint editors, and the student briefing view on the pod page, ship in follow-up UI releases. Until then, briefings can be authored only through the API calls below.

## Who sees what

| You attach it to | Students who see it |
| --- | --- |
| A Single VM template | The student who created a Single VM from that template |
| Any other template | Students whose pod has a VM cloned from that template, whether they deployed a blueprint or built the lab themselves |
| A blueprint | Students whose pod was deployed from that blueprint |

A pod can show several briefings. The blueprint briefing comes first, then one briefing per distinct template in the pod. A template briefing lists the pod's VMs that were cloned from it.

Before provisioning, students see only the template or blueprint **description**. There is no preview of the briefing in the catalog.

Instructors can read the briefing of any student's pod, so you see exactly what the student sees.

## Saving is not publishing

Every save creates a new, numbered, immutable **revision** with your name, the time and a short message. Saving never changes what students see.

Students see only the **published** revision. To show a revision, publish it. To roll back, publish an older revision. To hide the briefing, unpublish it.

Students always see the latest published revision, so a fix you publish reaches every student's next page load, including students who provisioned before the fix.

**Restore** copies an old revision into a new revision at the top of the history. It never rewrites history and does not publish anything.

## Two people editing at once

Every save names the revision it was based on. If someone else saved first, the API answers `409 Conflict` with the current `head_revision`. Your text is not lost: compare it with the newer revision, merge by hand, and save again on top of the new head.

## Rules for content

- **Never put passwords or other secrets in a briefing.** The pod page already shows each VM's credentials with copy buttons. A briefing is readable by every student whose pod uses the template or blueprint, and by every instructor.
- Briefings are limited to 256 KiB of Markdown.
- Markdown is rendered in the browser and sanitized. Raw HTML, scripts and `javascript:` links do not run.
- Obsidian-style callouts (`> [!tip]`, `> [!warning]`) and fenced code blocks work, as they do in this wiki.

## API

All instructor routes require the instructor role. `{owner}` is either `templates/{templateID}` or `blueprints/{blueprintID}`.

| Method | Path | Body | Result |
| --- | --- | --- | --- |
| GET | `/api/v1/admin/{owner}/briefing` | | `head_revision` (0 if never saved), `published_revision` (or null), `updated_at` |
| GET | `/api/v1/admin/{owner}/briefing/revisions` | | History, newest first, without bodies |
| GET | `/api/v1/admin/{owner}/briefing/revisions/{rev}` | | One revision including `body_md` |
| POST | `/api/v1/admin/{owner}/briefing/revisions` | `{"body_md": "...", "message": "...", "base_revision": 0}` | `201` with the new revision, `409` with `head_revision` if `base_revision` is stale |
| POST | `/api/v1/admin/{owner}/briefing/revisions/{rev}/restore` | | `201` with the new head revision (not published) |
| POST | `/api/v1/admin/{owner}/briefing/publish` | `{"revision": 3}` | The updated briefing summary |
| POST | `/api/v1/admin/{owner}/briefing/unpublish` | | The updated briefing summary |

`base_revision` is required on every save. Use `0` for the first save.

Students and instructors read a pod's published briefings with `GET /api/v1/pods/{podID}/briefing`. It returns `{"sections": [...]}`, with an empty list when nothing is published. Only the pod's owner and instructors can call it.

Every save, restore, publish and unpublish is written to the audit log.
