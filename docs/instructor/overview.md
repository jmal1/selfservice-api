# Crucible for Instructors — Overview

> [!note]
> This page is the **human-friendly tour**. If you want a deep, AI-ready
> reference for authoring workflows, see [AGENTS.md](../../AGENTS.md).
> If you want focused walkthroughs, see the other pages in this folder.

Crucible is a self-service VM lab. Students click **Deploy**, get an
isolated network with a Kali runner and one or more target VMs, and you
grade them with **assessments** that run automated checks.

This document explains how the pieces fit together.

Members of the **`lab-instructors`** group get the **full admin panel** —
Overview, Users, Templates, Blueprints, Actions, Workflows, Playlists, Runs,
VLAN Pool, Jobs, and Health — with one exception: the **Audit Log** (and the
active-Sessions view) stays admin-only. Your own actions are still recorded in
the audit log even though you cannot read it. If you hit a `403 Forbidden`
anywhere other than `/admin/audit`, report it.

---

## The mental model in one picture

```
            ┌──────────────┐
            │  TEMPLATE    │  A frozen VM image you can clone
            │  (one VM)    │  (e.g. "Ubuntu 24.04 hardened base")
            └──────┬───────┘
                   │  referenced by
                   ▼
            ┌──────────────┐
            │  BLUEPRINT   │  A recipe for a multi-VM lab
            │  (a pod)     │  (e.g. "1× Ubuntu target + 1× Windows target")
            └──────┬───────┘
                   │  deployed by a student =
                   ▼
            ┌──────────────┐
            │     POD      │  Live, isolated VMs on a dedicated VLAN
            │  (per-user)  │  (the student's playground)
            └──────┬───────┘
                   │  graded against
                   ▼
            ┌──────────────┐
            │   PLAYLIST   │  Ordered set of workflows
            │              │  (e.g. "Week 3: SSH Hardening Lab")
            └──────┬───────┘
                   │  composed of
                   ▼
            ┌──────────────┐
            │   WORKFLOW   │  One bash script = one assessment
            │              │  (e.g. "SSH password auth disabled")
            └──────┬───────┘
                   │  optionally calls
                   ▼
            ┌──────────────┐
            │    ACTION    │  Reusable one-step library item
            │   (library)  │  (e.g. "service-running")
            └──────────────┘
```

---

## The five nouns you'll meet

| Noun | What it is | Who owns it | Lives in |
|---|---|---|---|
| **Template** | A frozen vSphere VM image, ready to clone | Instructor / Admin | vCenter + Crucible DB |
| **Blueprint** | A recipe for a multi-VM pod (which templates, which NICs) | Instructor / Admin | Crucible DB |
| **Pod** | The actual cloned VMs running for one student | Student deploys, Instructor/Admin oversee | vCenter |
| **Workflow** | One bash script that grades one thing | **Instructor** | Crucible DB |
| **Playlist** | Ordered list of workflows, presented as a lab | **Instructor** | Crucible DB |
| **Action** | A reusable, parameterised one-step grading helper | Instructor / Admin | Crucible DB |

Most instructors spend most of their time writing **workflows** and occasionally
add reusable **actions** to the library. Templates, blueprints, users, and the
VLAN pool are infrastructure concerns you will usually touch less often.

---

## How a single student "Run Assessment" plays out

1. The student opens their pod page and clicks **Run** on a playlist.
2. The engine (`selfservice-engine`) provisions a runner pod — a Kali
   *container*, not a VM in the student's pod — on the student's VLAN.
   The runner has SSH access to every target VM in the pod. It is created
   for the run and destroyed when the run finishes; students never log
   into it and cannot schedule it.
3. For each workflow in the playlist (in order):
   - The engine writes the workflow's `script` into the runner and
     `bash`-executes it with the standard environment variables (see
     [Runner Environment](runner-environment.md)).
   - Every `run_action "<name>" ...` call inside the script is captured
     and stored — pass/fail, stdout, stderr, duration.
   - Anything the script prints with the `STUDENT_MSG:` prefix is
     shown to the student. Everything else is instructor-only.
4. Results are stored in the database. The student sees a pass/fail
   summary plus any student-facing messages.

The engine **does not** rerun parts of a workflow if one step fails. A workflow
is one bash process with `set -euo pipefail`; if grading steps need to be
independent, split them into separate workflows in the same playlist.

---

## What goes in a Workflow vs an Action vs a Playlist?

| Question | Answer |
|---|---|
| "I want to check **one specific thing once**" | Write a **workflow** with one inline `run_action` |
| "I'm going to check **the same kind of thing in many workflows**" (e.g. 'is a service running') | Add a library **action** with parameters, then call it from workflows |
| "I want to **assemble a full lab** for a class session" | Build a **playlist** by ordering the workflows you've already approved |

Start by writing workflows inline. Promote logic to a library action only when
you have copied it into a third workflow; premature generalisation hurts
maintainability.

---

## Two execution modes — and when to use which

Every workflow declares an `execution_mode`. There are two:

| Mode | Where the script runs | When to use |
|---|---|---|
| `kali_runner` (default) | Inside the **runner Kali pod** | Network checks (ports, services), SSH into target, fingerprinting |
| `vmware_tools` | **Inside the target VM** via VMware Tools | "Is this file present?", local config checks that don't need network |

> [!warning]
> `vmware_tools` mode requires the target VM to have VMware Tools
> installed and running. Most stock Linux/Windows images do. Kali by
> default does not — don't try to `vmware_tools`-execute against a Kali
> target.

Most assessments use `kali_runner`. See [Building Workflows](workflows.md) for
the practical details.

---

## The lifecycle of a workflow

```
draft  →  pending_review  →  approved  →  active
  │             │                │           │
  │             │                │           └── only `active` workflows
  │             │                │               can be added to a playlist run
  │             │                │
  │             │                └── reviewed and approved by an admin
  │             │
  │             └── you've submitted it for review
  │
  └── you're still authoring; not visible to playlists
```

Submit via the admin UI Workflows page, or via the lifecycle endpoints
(see [AGENTS.md](../../AGENTS.md) §6). Status changes are audited.

---

## Where to go next

| You want to… | Read… |
|---|---|
| Build a new VM template from scratch | [Building Templates](templates.md) |
| Follow a copy-me recipe for a specific OS | [Per-OS Template Build Recipes](os-recipes.md) |
| Author your first workflow | [Building Workflows](workflows.md) |
| Understand what's available to your script at runtime | [Runner Environment](runner-environment.md) |
| Add a reusable check to the library | [Building Actions](actions.md) |
| Bundle workflows into a graded lab | [Building Playlists](playlists.md) |
| Figure out why a workflow keeps failing | [Troubleshooting](troubleshooting.md) |
| Look up a term | [Glossary](glossary.md) |
| Review the student-facing lab guide | [Student Guide: Getting Started](../student/overview.md) |
| Give an AI coding agent everything in one shot | [AGENTS.md](../../AGENTS.md) |
