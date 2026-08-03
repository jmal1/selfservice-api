# Build-a-Workflow Prompt Template

Paste this prompt into **any** AI tool (ChatGPT, Claude.ai, Gemini, etc.) **immediately followed by the entire contents of `AGENTS.md`** to give the AI everything it needs to author a Crucible workflow or library action. Use this when you can't or don't want to point the AI at the repo directly.

---

## Copy from here

> You are helping me, a cybersecurity instructor, author content for **Crucible**, a self-service VM lab + automated assessment platform. The reference document immediately below this prompt (`AGENTS.md`) is the canonical source of truth for the workflow/action/playlist schema, the runner script contract, available library actions, bash conventions, and validation rules. **Treat its enum values, schema fields, and runtime conventions as absolute — never invent your own.**
>
> I will describe an assessment I want to build. You will produce a valid JSON payload for either `POST /api/v1/admin/workflows` (a complete workflow) or `POST /api/v1/admin/actions` (a single library action), depending on what I'm asking for.
>
> Follow this process:
>
> 1. **Clarify if the ask is ambiguous.** Ask at most one targeted question (e.g. "should this be an outside-the-VM check via Kali, or an inside-the-VM check via VMware Tools?") before producing the artifact. Don't ask multiple at once.
> 2. **Pick `execution_mode` deliberately.** Default to `kali_runner` unless the check fundamentally needs to be inside the target VM (e.g. checking a systemd unit, file ownership, registry key).
> 3. **Prefer reusing a library action over inlining the logic.** Check `AGENTS.md` §7 first. Only inline when no library action fits.
> 4. **Always include a `STUDENT_MSG:` line on every fail path.** Make it actionable — tell the student what to do, not just what failed.
> 5. **Quote every `$VAR`** in the bash script. Always set `set -euo pipefail`. Always `source /opt/crucible/lib/actions.sh` (kali_runner only).
> 6. **Add reasonable timeouts.** `timeout_seconds: 60` for actions, `300` for workflows is usually right; tighten for fast checks.
> 7. **Output the JSON in a code block, alone**, after a brief one-paragraph explanation of what it does and why you chose the design.
> 8. **Self-check before finalizing**, per `AGENTS.md` §12 — verify JSON parses, no invented enum values, slug uniqueness, shellcheck-clean bash, no leaked secrets, timeouts sane.
>
> My assessment idea:
>
> ```
> <PASTE YOUR IDEA HERE — e.g. "Verify the student configured fail2ban with maxretry=3 and that it's running on their Ubuntu target.">
> ```
>
> Reference document follows below:
>
> ---
>
> <PASTE THE ENTIRE CONTENTS OF AGENTS.md HERE>

## Copy to here

---

## Tips

- If you're using **Claude Code** or **Copilot CLI** in a clone of the `selfservice-api` repo, you don't need this prompt — the AI already picks up `AGENTS.md` automatically. Just describe what you want.
- If the AI's first attempt invents a field or uses an unknown `execution_mode`, paste `AGENTS.md` again and ask it to re-read §3.
- For complex multi-action workflows, ask for the workflow first, then iterate on individual `run_action` blocks one at a time.
- After the AI produces the JSON, you can paste it directly into the Admin → Workflows → "Import JSON" textarea, or `curl` it:

```bash
curl -X POST https://crucible.jmal.io/api/v1/admin/workflows \
  -H "Authorization: Bearer $CRUCIBLE_TOKEN" \
  -H "Content-Type: application/json" \
  -d @workflow.json
```
