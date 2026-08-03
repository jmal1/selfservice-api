# Glossary

Quick reference for the nouns and jargon that show up across Crucible
authoring docs.

| Term | Definition |
|---|---|
| **Action** | A reusable, parameterised one-step grading helper, stored in the action library. Called from workflows via `run_action "<slug>"`. See [Building Actions](actions.md). |
| **Action library** | The collection of registered actions, scoped to the deployment. Managed via `/api/v1/admin/actions`. |
| **Active** | A workflow or playlist status meaning "approved and usable in student-facing playlist runs". Final lifecycle state. |
| **Approved** | A workflow lifecycle state meaning "an admin reviewed and signed off, ready to activate". |
| **Assessment** | The act of running a playlist (or one-off workflow) against a student's pod. Produces a pass/fail report. |
| **Blueprint** | A multi-VM pod recipe, naming which templates to deploy and how they're networked. Students deploy pods *from* blueprints. |
| **Context** | A per-workflow key/value store that actions can read (`input_context`) and write (`output_context`). Lets one action's output influence the next. |
| **Creation mode** | `visual` (workflow built via the UI script builder) or `script` (raw bash author). |
| **Crucible** | This platform. The self-service VM lab and grading engine for cybersecurity instruction. |
| **Draft** | A workflow's initial lifecycle state. Not visible in any playlist run. |
| **Engine** | The `selfservice-engine` service that schedules and executes assessment runs. Lives at `selfservice-engine.selfservice.svc.cluster.local:8081`. |
| **Execution mode** | `kali_runner` (script runs on the Kali runner pod) or `vmware_tools` (script runs inside the target VM via guest tools). See [Runner Environment](runner-environment.md). |
| **Guest interpreter** | Only for `vmware_tools` mode: the binary inside the target that interprets the script. `/bin/bash`, `cmd.exe`, `powershell.exe`. |
| **Instructor** | A user role that can author workflows, actions, and playlists. |
| **Kali runner** | The Kali Linux pod the engine deploys onto the student's VLAN to execute workflows. Has SSH access to all targets in the pod. |
| **Library action** | Synonym for "action" — emphasises it's reusable, as opposed to inline bash inside a workflow. |
| **Pending review** | A workflow lifecycle state meaning "submitted by author, waiting on admin review". |
| **Platform tag** | An `os:distro` slug (e.g. `linux:ubuntu`, `windows:server2022`) that an action declares it supports. Engine refuses to run an action against an unsupported platform. |
| **Playlist** | Ordered list of workflows packaged as a single graded lab. See [Building Playlists](playlists.md). |
| **Pod** | A live, isolated set of VMs deployed for one student from a blueprint. Lives on its own VLAN. |
| **Required** | A `workflows[].required` flag on a playlist entry. If `false`, the entry's failure won't fail the overall playlist. |
| **Revision** | When you edit an active workflow or playlist, a new revision is created. In-flight runs use the version they launched with; new clicks pick up the new head. |
| **Run** | One execution of a workflow or playlist against a pod. Has a UUID exposed as `CRUCIBLE_RUN_ID` to scripts. |
| **Runner** | Synonym for "Kali runner". |
| **Script mode** | A workflow `creation_mode` where the author writes raw bash directly (vs the visual builder). |
| **`set -euo pipefail`** | The bash incantation you should put at the top of every workflow. Fails on errors, unset variables, and broken pipes. |
| **Setup script** | An optional workflow field that runs once before the main `script`. Hard 60s timeout. |
| **Slug** | A `[a-z0-9-]+` identifier, globally unique within its type. Used in URLs and `run_action` calls. |
| **`STUDENT_MSG:`** | Output line prefix that surfaces text to the student. Every other line is instructor-only. |
| **Target** | A VM in the student's pod that workflows grade. Identified by `CRUCIBLE_TARGET_IP` (primary) or `CRUCIBLE_TARGET_<SLOT>_IP` (additional). |
| **Template** | A frozen vSphere VM image, used as the basis for cloning targets and runners. Managed in vCenter + the Crucible templates table. |
| **Timeout** | Hard ceiling on a script's runtime. Per-workflow (`timeout_seconds`, default 300) and per-action (default 15). |
| **Visible to students** | A workflow/playlist flag. If `false`, the entry is hidden from the student UI but still runnable by instructors. |
| **VLAN** | The Layer-2 segment each pod gets, providing isolation between students. The runner and all targets share the pod's VLAN. |
| **Workflow** | One bash script = one assessment = one line item in the student's results panel. The thing you'll spend most authoring time on. |
| **`vmware_tools` mode** | An execution mode where the script runs inside the target VM via VMware Tools guest-ops API, not on the runner. |
