-- Crucible library action seed — the in-repo source of truth for the action
-- library.
--
-- WHY THIS FILE EXISTS
--
-- There was no seeding mechanism at all. The live library existed only in the
-- production database, mirrored in-repo as two hand-maintained test snapshots
-- that nothing forced to agree with it. A fresh environment came up with an
-- empty library, every workflow in it failed with exit 127, and migration
-- 000015's "Update existing seeded library actions" matched zero rows because
-- nothing had ever seeded them.
--
-- WHY NOT A MIGRATION
--
-- This inserts data rows, not schema. AGENTS.md §17 has the deploy validate the
-- migration chain for contiguity at an exact clean head before it will touch
-- production; putting an evolving content library in that chain would couple
-- "add three actions for next week's lab" to a gated release process.
--
-- WHY CONVERGENT (DO UPDATE) AND KEYED ON SLUG
--
-- Re-applying restores the committed definition rather than skipping, so a
-- hand-edit in the admin UI cannot leave the database quietly diverged from
-- version control. The conflict target is `slug` rather than a fixed UUID
-- because these rows already exist in production under ids this repo has never
-- seen; keying on slug adopts them instead of duplicating them. `idx_actions_slug`
-- is a partial unique index, so the inference clause must repeat its predicate.
--
-- WHY THE BODIES ARE DOLLAR-QUOTED
--
-- A body is bash, and bash is full of quotes and backslashes. Dollar quoting
-- passes it through byte for byte, which keeps this file diffable and lets
-- shellcheck read it. See .gitattributes: these bytes must be LF.
--
-- HOW TO APPLY
--
--   PW=$(kubectl -n selfservice get secret selfservice-db-creds \
--          -o jsonpath='{.data.password}' | base64 -d)
--   kubectl -n selfservice cp deploy/sql/library-actions.sql \
--     selfservice-postgresql-0:/tmp/library-actions.sql
--   kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U selfservice -d selfservice \
--     -f /tmp/library-actions.sql
--
-- Apply this BEFORE deploy/sql/library-workflows.sql: the workflows call these
-- actions, and that file validates that every callable it uses exists.
--
-- AUTHORING RULES (see AGENTS.md §2b and docs/instructor/actions.md)
--
--   * A body is rendered as `slug_with_underscores() { … }`, so use `local`
--     and `return`, never `exit`.
--   * Arguments arrive as `--flag value`; parse them with a while/case loop.
--     `params` and `PARAM_*` do not exist.
--   * Report failure by assigning LAST_ERROR (instructor) and
--     LAST_STUDENT_MSG (student), then `return 1`. `student_fail_hint` is inert.
--   * Anything that inspects the student's machine MUST go through
--     `crucible_ssh` / `crucible_ssh_sudo`. Calling `systemctl` or `dpkg`
--     directly inspects the Kali runner pod, which is the bug this seed fixes
--     in five previously shipped actions.
--   * Only call tools listed in internal/runnertools/tools.txt.

BEGIN;

-- crucible_seed_action keeps each entry below to its metadata plus its body.
-- Without it every action would repeat a 14-column INSERT and a 10-line
-- ON CONFLICT block, and the bash — the only part worth reviewing — would be
-- lost in the noise.
--
-- Dropped at the end of the transaction: this is a seeding aid, not an API,
-- and leaving it behind would invite someone to call it from application code.
CREATE OR REPLACE FUNCTION crucible_seed_action(
    p_slug        TEXT,
    p_name        TEXT,
    p_description TEXT,
    p_type        TEXT,
    p_category    TEXT,
    p_platforms   JSONB,
    p_input       JSONB,
    p_output      JSONB,
    p_script      TEXT
) RETURNS VOID AS $fn$
BEGIN
    INSERT INTO actions (
        name, slug, description, action_type, action_category,
        params, script, input_context, output_context,
        timeout_seconds, is_library, supported_platforms
    ) VALUES (
        p_name, p_slug, p_description, p_type, p_category,
        '{}'::jsonb, p_script, p_input, p_output,
        60, true, p_platforms
    )
    ON CONFLICT (slug) WHERE slug IS NOT NULL DO UPDATE SET
        name                = EXCLUDED.name,
        description         = EXCLUDED.description,
        action_type         = EXCLUDED.action_type,
        action_category     = EXCLUDED.action_category,
        script              = EXCLUDED.script,
        input_context       = EXCLUDED.input_context,
        output_context      = EXCLUDED.output_context,
        supported_platforms = EXCLUDED.supported_platforms,
        is_library          = true,
        updated_at          = now();
END;
$fn$ LANGUAGE plpgsql;


-- ===========================================================================
-- CORRECTED: actions that used to inspect the runner instead of the target
--
-- These five shipped in the original library running `systemctl`, `dpkg`,
-- `grep` and `sudo ufw` locally. Inside a kali_runner workflow that inspects
-- the Kali pod, so `service_running --name ssh` graded the runner's sshd and
-- `ufw_enabled` could only ever fail, because the runner is unprivileged and
-- has no sudo. Every one now reaches the student's VM over SSH.
-- ===========================================================================

SELECT crucible_seed_action(
    'service-running',
    'Service Running',
    'Asserts a systemd unit is active on the student VM.',
    'service_check', 'service',
    '["linux"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "systemd unit name, with or without .service"}]'::jsonb,
    '[{"key": "service_<name>_status", "type": "string", "description": "active, when the unit is running"}]'::jsonb,
$body$
local name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$name" ]]; then
    LAST_ERROR="service-running requires --name"
    return 2
fi
if crucible_ssh systemctl is-active --quiet "$name" >/dev/null 2>&1; then
    ctx_set "service_${name}_status" "active"
    return 0
fi
LAST_ERROR="Service $name is not active on $CRUCIBLE_TARGET_IP"
LAST_STUDENT_MSG="The $name service is not running. Start it with: sudo systemctl start $name"
return 1
$body$
);

SELECT crucible_seed_action(
    'service-enabled',
    'Service Enabled At Boot',
    'Asserts a systemd unit is enabled, so it survives a reboot.',
    'service_check', 'service',
    '["linux"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "systemd unit name"}]'::jsonb,
    '[]'::jsonb,
$body$
local name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$name" ]]; then
    LAST_ERROR="service-enabled requires --name"
    return 2
fi
# `is-enabled` is deliberately separate from `is-active`: a service started by
# hand passes is-active and disappears on the next reboot, which is the exact
# mistake this check is meant to catch.
if crucible_ssh systemctl is-enabled --quiet "$name" >/dev/null 2>&1; then
    return 0
fi
LAST_ERROR="Service $name is not enabled on $CRUCIBLE_TARGET_IP"
LAST_STUDENT_MSG="The $name service is not enabled at boot. Run: sudo systemctl enable $name"
return 1
$body$
);

SELECT crucible_seed_action(
    'service-stopped',
    'Service Stopped And Disabled',
    'Asserts a systemd unit is neither running nor enabled — for services that must be removed from a hardened host.',
    'service_check', 'service',
    '["linux"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "systemd unit name"}]'::jsonb,
    '[]'::jsonb,
$body$
local name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$name" ]]; then
    LAST_ERROR="service-stopped requires --name"
    return 2
fi
if crucible_ssh systemctl is-active --quiet "$name" >/dev/null 2>&1; then
    LAST_ERROR="Service $name is still active"
    LAST_STUDENT_MSG="The $name service is still running. Stop and disable it: sudo systemctl disable --now $name"
    return 1
fi
if crucible_ssh systemctl is-enabled --quiet "$name" >/dev/null 2>&1; then
    LAST_ERROR="Service $name is stopped but still enabled"
    LAST_STUDENT_MSG="The $name service is stopped but will come back on reboot. Run: sudo systemctl disable $name"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'package-installed',
    'Package Installed',
    'Asserts a dpkg package is installed on the student VM.',
    'service_check', 'service',
    '["linux:ubuntu", "linux:debian"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "package name as dpkg knows it"}]'::jsonb,
    '[]'::jsonb,
$body$
local name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$name" ]]; then
    LAST_ERROR="package-installed requires --name"
    return 2
fi
# dpkg-query -W -f '${Status}' is checked rather than `dpkg -l | grep ^ii`,
# because `dpkg -l` pads and truncates its columns to the terminal width and
# will silently mangle a long package name.
local status
status=$(crucible_ssh dpkg-query -W -f='${Status}' "$name" 2>/dev/null) || status=""
if [[ "$status" == *"install ok installed"* ]]; then
    return 0
fi
LAST_ERROR="Package $name is not installed on $CRUCIBLE_TARGET_IP (dpkg status: ${status:-absent})"
LAST_STUDENT_MSG="The $name package is not installed. Install it with: sudo apt install $name"
return 1
$body$
);

SELECT crucible_seed_action(
    'package-absent',
    'Package Not Installed',
    'Asserts a dpkg package is absent — for removing insecure services from a hardened host.',
    'service_check', 'service',
    '["linux:ubuntu", "linux:debian"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "package name as dpkg knows it"}]'::jsonb,
    '[]'::jsonb,
$body$
local name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$name" ]]; then
    LAST_ERROR="package-absent requires --name"
    return 2
fi
local status
status=$(crucible_ssh dpkg-query -W -f='${Status}' "$name" 2>/dev/null) || status=""
if [[ "$status" == *"install ok installed"* ]]; then
    LAST_ERROR="Package $name is still installed on $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="The $name package is still installed. Remove it with: sudo apt purge $name"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'file-contains',
    'File Contains',
    'Asserts a file on the student VM contains a literal string or matches a regex.',
    'file_check', 'file',
    '["linux"]'::jsonb,
    '[{"key": "path", "type": "string", "description": "absolute path on the target"},
      {"key": "value", "type": "string", "description": "literal string, or an extended regex when --regex is given"},
      {"key": "regex", "type": "boolean", "description": "flag with no value; treat --value as an extended regex"},
      {"key": "sudo", "type": "boolean", "description": "flag with no value; read the file as root"}]'::jsonb,
    '[]'::jsonb,
$body$
local path="" value="" regex=false use_sudo=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        --path) path="$2"; shift 2;;
        --value) value="$2"; shift 2;;
        --regex) regex=true; shift;;
        --sudo) use_sudo=true; shift;;
        *) shift;;
    esac
done
if [[ -z "$path" || -z "$value" ]]; then
    LAST_ERROR="file-contains requires --path and --value"
    return 2
fi
local -a grep_cmd=(grep -q)
if $regex; then grep_cmd+=(-E); else grep_cmd+=(-F); fi
grep_cmd+=(-- "$value" "$path")

local rc=0
if $use_sudo; then
    crucible_ssh_sudo test -r "$path" >/dev/null 2>&1 || rc=$?
else
    crucible_ssh test -r "$path" >/dev/null 2>&1 || rc=$?
fi
if [[ $rc -ne 0 ]]; then
    LAST_ERROR="Cannot read $path on $CRUCIBLE_TARGET_IP (ssh/test exit $rc)"
    LAST_STUDENT_MSG="The file $path does not exist or is not readable."
    return 1
fi

rc=0
if $use_sudo; then
    crucible_ssh_sudo "${grep_cmd[@]}" >/dev/null 2>&1 || rc=$?
else
    crucible_ssh "${grep_cmd[@]}" >/dev/null 2>&1 || rc=$?
fi
if [[ $rc -eq 0 ]]; then
    return 0
fi
LAST_ERROR="$path does not match '$value' on $CRUCIBLE_TARGET_IP"
LAST_STUDENT_MSG="Expected to find '$value' in $path"
return 1
$body$
);

SELECT crucible_seed_action(
    'ufw-enabled',
    'UFW Enabled',
    'Asserts the ufw firewall is active on the student VM.',
    'service_check', 'firewall',
    '["linux:ubuntu", "linux:debian"]'::jsonb,
    '[]'::jsonb,
    '[{"key": "ufw_status", "type": "string", "description": "active, when the firewall is up"}]'::jsonb,
$body$
# `ufw status` needs root even to read, so this is sudo on the TARGET. The
# original body ran `sudo ufw` on the runner, which is unprivileged and has no
# ufw installed — it could only ever fail.
local status
status=$(crucible_ssh_sudo ufw status 2>&1) || true
if [[ "$status" == *"Status: active"* ]]; then
    ctx_set "ufw_status" "active"
    return 0
fi
if [[ "$status" == *"a password is required"* || "$status" == *"not in the sudoers"* ]]; then
    LAST_ERROR="Cannot read ufw status: $CRUCIBLE_TARGET_USERNAME has no passwordless sudo on the target"
    LAST_STUDENT_MSG="This check could not read your firewall status because the assessment account lost sudo access. Please report it to your instructor."
    return 2
fi
LAST_ERROR="ufw is not active on $CRUCIBLE_TARGET_IP: $status"
LAST_STUDENT_MSG="The ufw firewall is not enabled. Run: sudo ufw enable"
return 1
$body$
);

SELECT crucible_seed_action(
    'ufw-rule-exists',
    'UFW Rule Exists',
    'Asserts ufw status lists a rule matching the given text.',
    'service_check', 'firewall',
    '["linux:ubuntu", "linux:debian"]'::jsonb,
    '[{"key": "rule", "type": "string", "description": "extended regex matched against a line of ufw status, e.g. \"22/tcp .*ALLOW\""}]'::jsonb,
    '[]'::jsonb,
$body$
local rule=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --rule) rule="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$rule" ]]; then
    LAST_ERROR="ufw-rule-exists requires --rule"
    return 2
fi
local status
status=$(crucible_ssh_sudo ufw status 2>&1) || true
if [[ "$status" == *"a password is required"* || "$status" == *"not in the sudoers"* ]]; then
    LAST_ERROR="Cannot read ufw status: no passwordless sudo on the target"
    LAST_STUDENT_MSG="This check could not read your firewall rules because the assessment account lost sudo access. Please report it to your instructor."
    return 2
fi
if grep -qE -- "$rule" <<<"$status"; then
    return 0
fi
LAST_ERROR="No ufw rule matching '$rule' on $CRUCIBLE_TARGET_IP"
LAST_STUDENT_MSG="No firewall rule matches '$rule'. Check your rules with: sudo ufw status numbered"
return 1
$body$
);

SELECT crucible_seed_action(
    'ufw-default-deny-incoming',
    'UFW Default Deny Incoming',
    'Asserts the ufw default incoming policy is deny — the setting that makes per-port rules an allowlist rather than decoration.',
    'service_check', 'firewall',
    '["linux:ubuntu", "linux:debian"]'::jsonb,
    '[]'::jsonb,
    '[]'::jsonb,
$body$
local status
status=$(crucible_ssh_sudo ufw status verbose 2>&1) || true
if [[ "$status" == *"a password is required"* || "$status" == *"not in the sudoers"* ]]; then
    LAST_ERROR="Cannot read ufw status: no passwordless sudo on the target"
    LAST_STUDENT_MSG="This check could not read your firewall policy because the assessment account lost sudo access. Please report it to your instructor."
    return 2
fi
# The verbose line reads e.g. "Default: deny (incoming), allow (outgoing), ...".
if grep -qE '^Default:[[:space:]]+deny[[:space:]]+\(incoming\)' <<<"$status"; then
    return 0
fi
LAST_ERROR="ufw default incoming policy is not deny on $CRUCIBLE_TARGET_IP"
LAST_STUDENT_MSG="Your firewall still accepts incoming traffic by default. Run: sudo ufw default deny incoming"
return 1
$body$
);


-- ===========================================================================
-- PRE-EXISTING: the library as it shipped, adopted by slug
--
-- These nineteen already exist in the production database under ids this repo
-- has never seen, so the ON CONFLICT (slug) clause adopts them rather than
-- duplicating them. They are captured here because the seed has to be the
-- WHOLE library: a partial seed brings up a fresh environment where the
-- workflows in library-workflows.sql call port_open and http_get and get
-- exit 127.
--
-- Bodies are reproduced verbatim from what shipped, apart from a trailing
-- newline that dollar quoting adds. The renderer wraps a body in
-- `slug() { … }` either way, so that is not a behaviour change. Only the five
-- corrected above have different logic.
--
-- Descriptions for the six Windows entries were written here: they had none in
-- production, which left them blank in the admin UI's library browser.
--
-- Note that command-check and ssh-exec deliberately run on the RUNNER, not on
-- the target: command_check is what the runner_smoke synthetic uses to inspect
-- the runner's own net1 interface, and ssh-exec takes an explicit --host. That
-- is not the bug the five corrected actions had -- those claimed to inspect
-- the student's VM and did not.
-- ===========================================================================

SELECT crucible_seed_action(
    'command-check',
    'Command Check',
    'Runs an arbitrary command and asserts output and exit code',
    'command', 'general',
    '["linux"]'::jsonb,
    '[{"key": "cmd", "type": "string", "description": "Shell command to execute"}, {"key": "expect_output", "type": "string", "description": "Expected substring in output"}, {"key": "expect_exit", "type": "number", "description": "Expected exit code (default 0)"}]'::jsonb,
    '[{"key": "command_output", "type": "string", "description": "Command stdout+stderr"}, {"key": "command_exit_code", "type": "number", "description": "Exit code"}]'::jsonb,
$body$
local cmd="" expect_output="" expect_exit=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --cmd) cmd="$2"; shift 2;;
        --expect-output) expect_output="$2"; shift 2;;
        --expect-exit) expect_exit="$2"; shift 2;;
        *) shift;;
    esac
done
local output exit_code=0
output=$(eval "$cmd" 2>&1) || exit_code=$?
ctx_set "command_output" "$output"
ctx_set "command_exit_code" "$exit_code"
if [[ "$exit_code" != "$expect_exit" ]]; then
    LAST_ERROR="Command exited $exit_code (expected $expect_exit)"
    return 1
fi
if [[ -n "$expect_output" && "$output" != *"$expect_output"* ]]; then
    LAST_ERROR="Output does not contain '$expect_output'"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'dns-resolves',
    'DNS Resolution',
    'Verifies a hostname resolves via DNS, optionally to a specific IP',
    'dns', 'network',
    '["any"]'::jsonb,
    '[{"key": "hostname", "type": "string", "description": "Hostname to resolve"}, {"key": "server", "type": "string", "description": "DNS server to query (optional)"}, {"key": "expect_ip", "type": "string", "description": "Expected IP address (optional)"}]'::jsonb,
    '[{"key": "dns_result", "type": "string", "description": "Resolved IP address"}]'::jsonb,
$body$
local hostname="" server="" expect_ip=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --hostname) hostname="$2"; shift 2;;
        --server) server="$2"; shift 2;;
        --expect-ip) expect_ip="$2"; shift 2;;
        *) shift;;
    esac
done
local result
if [[ -n "$server" ]]; then
    result=$(dig +short "$hostname" @"$server" 2>/dev/null | head -1)
else
    result=$(dig +short "$hostname" 2>/dev/null | head -1)
fi
ctx_set "dns_result" "$result"
if [[ -z "$result" ]]; then
    LAST_ERROR="DNS lookup failed for $hostname"
    LAST_STUDENT_MSG="$hostname does not resolve. Check your DNS configuration."
    return 1
fi
if [[ -n "$expect_ip" && "$result" != "$expect_ip" ]]; then
    LAST_ERROR="$hostname resolved to $result, expected $expect_ip"
    LAST_STUDENT_MSG="$hostname resolves to $result but should resolve to $expect_ip"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'git-clone',
    'Git Clone',
    'Clones a git repository and verifies success',
    'command', 'general',
    '["linux"]'::jsonb,
    '[{"key": "url", "type": "string", "description": "Git repository URL"}, {"key": "dest", "type": "string", "description": "Clone destination path (optional)"}]'::jsonb,
    '[{"key": "git_repo_path", "type": "string", "description": "Path to cloned repo"}]'::jsonb,
$body$
local url="" dest=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url) url="$2"; shift 2;;
        --dest) dest="$2"; shift 2;;
        *) shift;;
    esac
done
dest="${dest:-$CRUCIBLE_WORKDIR/repo}"
if git clone "$url" "$dest" 2>/dev/null; then
    ctx_set "git_repo_path" "$dest"
    return 0
else
    LAST_ERROR="Git clone failed: $url"
    LAST_STUDENT_MSG="Cannot clone from $url. Is the Git server running?"
    return 1
fi
$body$
);

SELECT crucible_seed_action(
    'git-push',
    'Git Push',
    'Creates a commit and pushes to a remote repository',
    'command', 'general',
    '["linux"]'::jsonb,
    '[{"key": "repo", "type": "string", "description": "Repo path (or uses git_repo_path from context)"}, {"key": "file", "type": "string", "description": "File to modify"}, {"key": "content", "type": "string", "description": "Content to append"}, {"key": "message", "type": "string", "description": "Commit message"}, {"key": "branch", "type": "string", "description": "Branch name (default: main)"}]'::jsonb,
    '[]'::jsonb,
$body$
local repo="" file="" content="" message="" branch="main"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --repo) repo="$2"; shift 2;;
        --file) file="$2"; shift 2;;
        --content) content="$2"; shift 2;;
        --message) message="$2"; shift 2;;
        --branch) branch="$2"; shift 2;;
        *) shift;;
    esac
done
repo="${repo:-$(ctx_get git_repo_path)}"
cd "$repo" || return 1
echo "$content" >> "$file"
git add "$file" && git commit -m "$message" 2>/dev/null && git push origin "$branch" 2>/dev/null
local rc=$?
if [ $rc -ne 0 ]; then
    LAST_ERROR="Git push failed"
    LAST_STUDENT_MSG="Could not push to remote. Check authentication and permissions."
fi
return $rc
$body$
);

SELECT crucible_seed_action(
    'http-get',
    'HTTP GET Check',
    'Makes an HTTP GET request and asserts status code and/or body content',
    'http', 'network',
    '["any"]'::jsonb,
    '[{"key": "url", "type": "string", "description": "URL to request"}, {"key": "expect_status", "type": "string", "description": "Expected HTTP status code"}, {"key": "expect_body", "type": "string", "description": "Expected substring in response body"}, {"key": "cookies", "type": "string", "description": "Cookie jar name to send"}, {"key": "save_cookies", "type": "string", "description": "Cookie jar name to save to"}]'::jsonb,
    '[{"key": "last_http_status", "type": "string", "description": "HTTP status code received"}, {"key": "last_http_body_length", "type": "number", "description": "Response body length"}, {"key": "last_http_url", "type": "string", "description": "Requested URL"}]'::jsonb,
$body$
local url="" expect_status="" expect_body="" cookies="" save_cookies="" timeout=10
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url) url="$2"; shift 2;;
        --expect-status) expect_status="$2"; shift 2;;
        --expect-body) expect_body="$2"; shift 2;;
        --cookies) cookies="$2"; shift 2;;
        --save-cookies) save_cookies="$2"; shift 2;;
        --timeout) timeout="$2"; shift 2;;
        *) shift;;
    esac
done

local curl_args=(-s -o /tmp/response_body -w "%{http_code}" --max-time "$timeout")
[[ -n "$cookies" ]] && curl_args+=(-b "/tmp/cookies_${cookies}")
[[ -n "$save_cookies" ]] && curl_args+=(-c "/tmp/cookies_${save_cookies}")

local status_code
status_code=$(curl "${curl_args[@]}" "$url")
local body=$(cat /tmp/response_body)

ctx_set "last_http_status" "$status_code"
ctx_set "last_http_body_length" "${#body}"
ctx_set "last_http_url" "$url"

if [[ -n "$expect_status" && "$status_code" != "$expect_status" ]]; then
    LAST_ERROR="Expected HTTP $expect_status, got $status_code"
    LAST_STUDENT_MSG="Web server returned $status_code instead of $expect_status for $url"
    return 1
fi
if [[ -n "$expect_body" && "$body" != *"$expect_body"* ]]; then
    LAST_ERROR="Response body does not contain '$expect_body'"
    LAST_STUDENT_MSG="Expected '$expect_body' in the response from $url"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'http-post',
    'HTTP POST',
    'Makes an HTTP POST request with form data or JSON',
    'http', 'network',
    '["any"]'::jsonb,
    '[{"key": "url", "type": "string", "description": "URL to POST to"}, {"key": "data", "type": "string", "description": "Form data or JSON body"}, {"key": "expect_status", "type": "string", "description": "Expected HTTP status code"}]'::jsonb,
    '[{"key": "last_http_status", "type": "string", "description": "HTTP status code received"}, {"key": "last_http_url", "type": "string", "description": "Requested URL"}]'::jsonb,
$body$
local url="" data="" expect_status="" cookies="" save_cookies="" content_type="application/x-www-form-urlencoded"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url) url="$2"; shift 2;;
        --data) data="$2"; shift 2;;
        --json) data="$2"; content_type="application/json"; shift 2;;
        --expect-status) expect_status="$2"; shift 2;;
        --cookies) cookies="$2"; shift 2;;
        --save-cookies) save_cookies="$2"; shift 2;;
        *) shift;;
    esac
done

local curl_args=(-s -o /tmp/response_body -w "%{http_code}" -X POST)
curl_args+=(-H "Content-Type: $content_type" -d "$data")
[[ -n "$cookies" ]] && curl_args+=(-b "/tmp/cookies_${cookies}")
[[ -n "$save_cookies" ]] && curl_args+=(-c "/tmp/cookies_${save_cookies}")

local status_code
status_code=$(curl "${curl_args[@]}" "$url")

ctx_set "last_http_status" "$status_code"
ctx_set "last_http_url" "$url"

if [[ -n "$expect_status" && "$status_code" != "$expect_status" ]]; then
    LAST_ERROR="Expected HTTP $expect_status, got $status_code from POST $url"
    LAST_STUDENT_MSG="Form submission returned $status_code instead of $expect_status"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'nmap-service',
    'Nmap Service Scan',
    'Scans a port with nmap to identify running services',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "Target hostname or IP"}, {"key": "port", "type": "number", "description": "Port to scan"}, {"key": "expect_service", "type": "string", "description": "Expected service name (optional)"}]'::jsonb,
    '[{"key": "nmap_output", "type": "string", "description": "Full nmap output"}]'::jsonb,
$body$
local host="" port="" expect_service=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        --expect-service) expect_service="$2"; shift 2;;
        *) shift;;
    esac
done
local output
output=$(nmap -sV -p "$port" "$host" 2>/dev/null)
ctx_set "nmap_output" "$output"
if [[ -n "$expect_service" ]] && echo "$output" | grep -qi "$expect_service"; then
    return 0
elif [[ -z "$expect_service" ]] && echo "$output" | grep -q "open"; then
    return 0
else
    LAST_ERROR="Service not found on $host:$port"
    LAST_STUDENT_MSG="Expected service '$expect_service' on port $port but it's not running."
    return 1
fi
$body$
);

SELECT crucible_seed_action(
    'port-closed',
    'Port Closed Check',
    'Verifies a TCP port is closed (firewall blocking)',
    'port_check', 'firewall',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "Target hostname or IP"}, {"key": "port", "type": "number", "description": "TCP port to check"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="" port="" timeout=3
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        *) shift;;
    esac
done
if ! timeout "$timeout" bash -c "echo >/dev/tcp/$host/$port" 2>/dev/null; then
    return 0
else
    LAST_ERROR="Port $port is open on $host (should be closed)"
    LAST_STUDENT_MSG="Port $port is still open. Configure the firewall to block it."
    return 1
fi
$body$
);

SELECT crucible_seed_action(
    'port-open',
    'Port Open Check',
    'Verifies a TCP port is open and accepting connections',
    'port_check', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "Target hostname or IP"}, {"key": "port", "type": "number", "description": "TCP port to check"}]'::jsonb,
    '[{"key": "port_N_open", "type": "boolean", "description": "Whether the port is open"}]'::jsonb,
$body$
local host="" port="" timeout=5
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        --timeout) timeout="$2"; shift 2;;
        *) shift;;
    esac
done
if timeout "$timeout" bash -c "echo >/dev/tcp/$host/$port" 2>/dev/null; then
    ctx_set "port_${port}_open" "true"
    return 0
else
    LAST_ERROR="Port $port is not open on $host"
    LAST_STUDENT_MSG="Port $port is not responding on $host. Is the service running?"
    return 1
fi
$body$
);

SELECT crucible_seed_action(
    'smb-share-accessible',
    'SMB Share Check',
    'Checks if an SMB share is accessible',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "SMB server hostname or IP"}, {"key": "share", "type": "string", "description": "Share name"}, {"key": "user", "type": "string", "description": "Username (optional for anonymous)"}, {"key": "pass", "type": "string", "description": "Password (optional)"}]'::jsonb,
    '[{"key": "smb_accessible", "type": "boolean", "description": "Whether the share was accessible"}]'::jsonb,
$body$
local host="" share="" user="" pass=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --share) share="$2"; shift 2;;
        --user) user="$2"; shift 2;;
        --pass) pass="$2"; shift 2;;
        *) shift;;
    esac
done
local output
if [[ -n "$user" ]]; then
    output=$(smbclient "//$host/$share" -U "$user%$pass" -c "ls" 2>&1)
else
    output=$(smbclient "//$host/$share" -N -c "ls" 2>&1)
fi
if echo "$output" | grep -q "blocks"; then
    ctx_set "smb_accessible" "true"
    return 0
else
    LAST_ERROR="SMB share //$host/$share not accessible"
    LAST_STUDENT_MSG="Cannot access SMB share //$host/$share. Check share permissions."
    return 1
fi
$body$
);

SELECT crucible_seed_action(
    'ssh-exec',
    'SSH Command',
    'Runs a command on a remote host via SSH and asserts output/exit code',
    'ssh', 'ssh',
    '["linux"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "Target hostname or IP"}, {"key": "user", "type": "string", "description": "SSH username"}, {"key": "command", "type": "string", "description": "Command to run"}, {"key": "expect_output", "type": "string", "description": "Expected substring in output"}, {"key": "expect_exit", "type": "number", "description": "Expected exit code (default 0)"}]'::jsonb,
    '[{"key": "ssh_output", "type": "string", "description": "Command output"}, {"key": "ssh_exit_code", "type": "number", "description": "Exit code"}]'::jsonb,
$body$
local host="" user="" command="" expect_output="" expect_exit=0 key=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --user) user="$2"; shift 2;;
        --command) command="$2"; shift 2;;
        --expect-output) expect_output="$2"; shift 2;;
        --expect-exit) expect_exit="$2"; shift 2;;
        --key) key="$2"; shift 2;;
        *) shift;;
    esac
done
local ssh_args=(-o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes)
[[ -n "$key" ]] && ssh_args+=(-i "$key")
local output exit_code
output=$(ssh "${ssh_args[@]}" "${user}@${host}" "$command" 2>&1) || exit_code=$?
exit_code=${exit_code:-0}
ctx_set "ssh_output" "$output"
ctx_set "ssh_exit_code" "$exit_code"
if [[ "$exit_code" != "$expect_exit" ]]; then
    LAST_ERROR="SSH command exited $exit_code (expected $expect_exit): $output"
    LAST_STUDENT_MSG="Command failed on $host: $command"
    return 1
fi
if [[ -n "$expect_output" && "$output" != *"$expect_output"* ]]; then
    LAST_ERROR="SSH output does not contain '$expect_output': $output"
    LAST_STUDENT_MSG="Expected '$expect_output' in the output of: $command"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'ssh-denied',
    'SSH Login Denied',
    'Verifies SSH login is denied for a specific user',
    'ssh', 'ssh',
    '["linux"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "Target hostname or IP"}, {"key": "user", "type": "string", "description": "Username that should be denied"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="" user=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --user) user="$2"; shift 2;;
        *) shift;;
    esac
done
if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes "${user}@${host}" exit 2>/dev/null; then
    LAST_ERROR="SSH login succeeded for $user@$host (should be denied)"
    LAST_STUDENT_MSG="SSH login as $user is still allowed. Disable it in sshd_config."
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'wait-for',
    'Wait For Condition',
    'Retries a command until it succeeds or times out',
    'command', 'general',
    '["linux"]'::jsonb,
    '[{"key": "description", "type": "string", "description": "Human-readable description of the condition"}, {"key": "cmd", "type": "string", "description": "Shell command to evaluate"}, {"key": "retries", "type": "number", "description": "Max attempts (default 10)"}, {"key": "delay", "type": "number", "description": "Seconds between retries (default 2)"}]'::jsonb,
    '[]'::jsonb,
$body$
local description="" cmd="" retries=10 delay=2
while [[ $# -gt 0 ]]; do
    case "$1" in
        --description) description="$2"; shift 2;;
        --cmd) cmd="$2"; shift 2;;
        --retries) retries="$2"; shift 2;;
        --delay) delay="$2"; shift 2;;
        *) shift;;
    esac
done
for i in $(seq 1 "$retries"); do
    if eval "$cmd" 2>/dev/null; then
        return 0
    fi
    sleep "$delay"
done
LAST_ERROR="Timed out waiting for: $description"
LAST_STUDENT_MSG="$description did not become ready after ${retries} attempts."
return 1
$body$
);

SELECT crucible_seed_action(
    'win-command-check',
    'Windows Command Check',
    'Runs a PowerShell command in the guest and asserts output and exit code',
    'command', 'general',
    '["windows"]'::jsonb,
    '[{"key": "cmd", "type": "string", "description": "PowerShell command to execute"}, {"key": "expect_output", "type": "string", "description": "Expected substring in output"}, {"key": "expect_exit", "type": "number", "description": "Expected exit code (default 0)"}]'::jsonb,
    '[{"key": "command_output", "type": "string", "description": "Command output"}, {"key": "command_exit_code", "type": "number", "description": "Exit code"}]'::jsonb,
$body$
$cmd = ""; $expectOutput = ""; $expectExit = 0
for ($i=0; $i -lt $args.Count; $i++) {
    switch ($args[$i]) {
        "--cmd" { $cmd = $args[++$i] }
        "--expect-output" { $expectOutput = $args[++$i] }
        "--expect-exit" { $expectExit = [int]$args[++$i] }
    }
}
try {
    $output = Invoke-Expression $cmd 2>&1 | Out-String
    $exitCode = $LASTEXITCODE
    if ($null -eq $exitCode) { $exitCode = 0 }
} catch {
    $output = $_.Exception.Message
    $exitCode = 1
}
ctx_set "command_output" $output
ctx_set "command_exit_code" $exitCode
if ($exitCode -ne $expectExit) {
    $env:LAST_ERROR = "Command exited $exitCode (expected $expectExit)"
    exit 1
}
if ($expectOutput -and $output -notlike "*$expectOutput*") {
    $env:LAST_ERROR = "Output does not contain '$expectOutput'"
    exit 1
}
exit 0
$body$
);

SELECT crucible_seed_action(
    'win-file-contains',
    'Windows File Contains',
    'Checks a file in the Windows guest contains a string or regex pattern',
    'file_check', 'file',
    '["windows"]'::jsonb,
    '[{"key": "path", "type": "string", "description": "File path"}, {"key": "value", "type": "string", "description": "String or regex to find"}, {"key": "regex", "type": "boolean", "description": "Use regex (default: false)"}]'::jsonb,
    '[]'::jsonb,
$body$
$path = ""; $value = ""; $regex = $false
for ($i=0; $i -lt $args.Count; $i++) {
    switch ($args[$i]) {
        "--path" { $path = $args[++$i] }
        "--value" { $value = $args[++$i] }
        "--regex" { $regex = $true }
    }
}
if (-not (Test-Path $path)) {
    $env:LAST_ERROR = "File not found: $path"
    $env:LAST_STUDENT_MSG = "File $path does not exist."
    exit 1
}
if ($regex) {
    $match = Select-String -Path $path -Pattern $value -Quiet
} else {
    $match = Select-String -Path $path -SimpleMatch $value -Quiet
}
if ($match) { exit 0 }
$env:LAST_ERROR = "File $path does not contain '$value'"
$env:LAST_STUDENT_MSG = "Expected '$value' in $path"
exit 1
$body$
);

SELECT crucible_seed_action(
    'win-firewall-enabled',
    'Windows Firewall Enabled',
    'Checks the Windows firewall is enabled for a profile',
    'service_check', 'firewall',
    '["windows"]'::jsonb,
    '[]'::jsonb,
    '[{"key": "firewall_status", "type": "string", "description": "enabled if any profile is on"}, {"key": "firewall_profiles", "type": "string", "description": "Comma-separated active profile names"}]'::jsonb,
$body$
$profiles = Get-NetFirewallProfile | Where-Object { $_.Enabled -eq $true }
if ($profiles.Count -ge 1) {
    ctx_set "firewall_status" "enabled"
    ctx_set "firewall_profiles" ($profiles.Name -join ",")
    exit 0
} else {
    $env:LAST_ERROR = "Windows Firewall is not enabled"
    $env:LAST_STUDENT_MSG = "Windows Firewall is disabled. Enable it in Windows Security settings."
    exit 1
}
$body$
);

SELECT crucible_seed_action(
    'win-firewall-rule-exists',
    'Windows Firewall Rule Exists',
    'Checks a named Windows firewall rule exists and is enabled',
    'service_check', 'firewall',
    '["windows"]'::jsonb,
    '[{"key": "rule", "type": "string", "description": "Firewall rule display name pattern"}]'::jsonb,
    '[]'::jsonb,
$body$
$ruleName = ""
for ($i=0; $i -lt $args.Count; $i++) {
    if ($args[$i] -eq "--rule") { $ruleName = $args[++$i] }
}
$rule = Get-NetFirewallRule -DisplayName "*$ruleName*" -ErrorAction SilentlyContinue | Where-Object { $_.Enabled -eq "True" }
if ($rule) { exit 0 }
$env:LAST_ERROR = "Firewall rule not found: $ruleName"
$env:LAST_STUDENT_MSG = "Missing or disabled firewall rule: $ruleName"
exit 1
$body$
);

SELECT crucible_seed_action(
    'win-package-installed',
    'Windows Package Installed',
    'Checks a program is present in the Windows installed-software list',
    'service_check', 'service',
    '["windows"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "Program/package name to search for"}]'::jsonb,
    '[]'::jsonb,
$body$
$name = ""
for ($i=0; $i -lt $args.Count; $i++) {
    if ($args[$i] -eq "--name") { $name = $args[++$i] }
}
$found = Get-Package -Name "*$name*" -ErrorAction SilentlyContinue
if (-not $found) {
    $found = Get-ItemProperty "HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*" |
        Where-Object { $_.DisplayName -like "*$name*" }
}
if ($found) { exit 0 }
$env:LAST_ERROR = "Package $name is not installed"
$env:LAST_STUDENT_MSG = "Package $name is not installed on this Windows system."
exit 1
$body$
);

SELECT crucible_seed_action(
    'win-service-running',
    'Windows Service Running',
    'Checks a Windows service is in the Running state',
    'service_check', 'service',
    '["windows"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "Windows service name"}]'::jsonb,
    '[{"key": "service_NAME_status", "type": "string", "description": "Service status"}]'::jsonb,
$body$
$name = ""
foreach ($arg in $args) {
    if ($arg -eq "--name") { $name = $args[$args.IndexOf($arg)+1] }
}
$svc = Get-Service -Name $name -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -eq "Running") {
    ctx_set "service_${name}_status" "Running"
    exit 0
} else {
    $env:LAST_ERROR = "Service $name is not running"
    $env:LAST_STUDENT_MSG = "Service $name is not running. Start it in services.msc or with: Start-Service $name"
    exit 1
}
$body$
);


-- ===========================================================================
-- HARDENING — SSH daemon configuration
--
-- All of these read `sshd -T`, the daemon's own effective configuration dump,
-- rather than grepping /etc/ssh/sshd_config. Grepping the file gets the wrong
-- answer in three common ways: a directive can be overridden later in the
-- file, it can be set in an Include'd drop-in under sshd_config.d (which is
-- how Ubuntu 22.04+ ships), and a commented-out line still matches a careless
-- regex. `sshd -T` reports what the running daemon will actually enforce, and
-- prints every key in lowercase.
-- ===========================================================================

SELECT crucible_seed_action(
    'sshd-directive',
    'SSHD Effective Directive',
    'Asserts sshd''s effective configuration sets a directive to an expected value. Reads `sshd -T`, so Include''d drop-ins and later overrides are accounted for.',
    'ssh', 'ssh',
    '["linux"]'::jsonb,
    '[{"key": "directive", "type": "string", "description": "directive name, case-insensitive, e.g. permitrootlogin"},
      {"key": "value", "type": "string", "description": "expected value, compared case-insensitively"}]'::jsonb,
    '[{"key": "sshd_<directive>", "type": "string", "description": "the effective value that was found"}]'::jsonb,
$body$
local directive="" value=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --directive) directive="$2"; shift 2;;
        --value) value="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$directive" || -z "$value" ]]; then
    LAST_ERROR="sshd-directive requires --directive and --value"
    return 2
fi
local lower_directive lower_value dump found
lower_directive="${directive,,}"
lower_value="${value,,}"

# `sshd -T` requires root because it reads the host keys.
dump=$(crucible_ssh_sudo sshd -T 2>&1) || dump=""
if [[ -z "$dump" ]]; then
    LAST_ERROR="Could not read sshd effective config from $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="This check could not read your SSH server configuration. Make sure the ssh service is installed and running."
    return 1
fi
found=$(grep -iE "^${lower_directive}[[:space:]]" <<<"$dump" | head -1 | cut -d' ' -f2- | tr -d '\r')
ctx_set "sshd_${lower_directive}" "$found"
if [[ "${found,,}" == "$lower_value" ]]; then
    return 0
fi
LAST_ERROR="sshd $directive is '${found:-unset}', expected '$value'"
LAST_STUDENT_MSG="SSH setting $directive should be '$value' but is '${found:-unset}'. Edit /etc/ssh/sshd_config (or a file in /etc/ssh/sshd_config.d/), then run: sudo systemctl reload ssh"
return 1
$body$
);

SELECT crucible_seed_action(
    'sshd-protocol-hardened',
    'SSH Root Login And Password Auth Disabled',
    'Asserts the two SSH settings that matter most: root cannot log in, and password authentication is off.',
    'ssh', 'ssh',
    '["linux"]'::jsonb,
    '[]'::jsonb,
    '[]'::jsonb,
$body$
local dump
dump=$(crucible_ssh_sudo sshd -T 2>&1) || dump=""
if [[ -z "$dump" ]]; then
    LAST_ERROR="Could not read sshd effective config from $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="This check could not read your SSH server configuration. Make sure the ssh service is installed and running."
    return 1
fi
local root_login password_auth problems=""
root_login=$(grep -iE '^permitrootlogin[[:space:]]' <<<"$dump" | head -1 | awk '{print $2}' | tr -d '\r')
password_auth=$(grep -iE '^passwordauthentication[[:space:]]' <<<"$dump" | head -1 | awk '{print $2}' | tr -d '\r')

# `prohibit-password` still permits key-based root login. It is a real
# hardening step but not the one being asserted here, so it is called out
# rather than quietly accepted.
if [[ "${root_login,,}" != "no" ]]; then
    problems="PermitRootLogin is '${root_login:-unset}' (want no)"
fi
if [[ "${password_auth,,}" != "no" ]]; then
    [[ -n "$problems" ]] && problems="$problems; "
    problems="${problems}PasswordAuthentication is '${password_auth:-unset}' (want no)"
fi
if [[ -z "$problems" ]]; then
    return 0
fi
LAST_ERROR="sshd is not hardened on $CRUCIBLE_TARGET_IP: $problems"
LAST_STUDENT_MSG="Your SSH server is not hardened: $problems. Fix it in /etc/ssh/sshd_config, then run: sudo systemctl reload ssh"
return 1
$body$
);

SELECT crucible_seed_action(
    'ssh-key-auth-only',
    'SSH Rejects Password Login',
    'Proves from the network that sshd refuses password authentication, rather than trusting the config file.',
    'ssh', 'ssh',
    '["linux"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "user", "type": "string", "description": "account to attempt, defaults to root"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" user="root"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --user) user="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="ssh-key-auth-only requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
# Asking for password auth explicitly and reading what the server offers back.
# A server with PasswordAuthentication no answers "Permission denied
# (publickey)" -- the method list is the evidence, and it does not require
# actually knowing a password.
local out
out=$(ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
         -o ConnectTimeout=5 -o LogLevel=ERROR -o BatchMode=yes \
         -o PreferredAuthentications=password -o PubkeyAuthentication=no \
         "${user}@${host}" true 2>&1) || true
if grep -qiE 'permission denied \(publickey\)' <<<"$out"; then
    return 0
fi
if grep -qi 'connection refused\|no route to host\|timed out' <<<"$out"; then
    LAST_ERROR="Could not reach sshd on $host: $out"
    LAST_STUDENT_MSG="Could not connect to SSH on $host. Is the ssh service running and allowed through the firewall?"
    return 1
fi
LAST_ERROR="sshd on $host still offers password authentication: $out"
LAST_STUDENT_MSG="Your SSH server still accepts password logins. Set PasswordAuthentication no in /etc/ssh/sshd_config and reload ssh."
return 1
$body$
);


-- ===========================================================================
-- HARDENING — accounts, permissions, and kernel settings
-- ===========================================================================

SELECT crucible_seed_action(
    'user-exists',
    'User Account Exists',
    'Asserts a local account is present in /etc/passwd on the student VM.',
    'command', 'system',
    '["linux"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "login name"}]'::jsonb,
    '[]'::jsonb,
$body$
local name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$name" ]]; then
    LAST_ERROR="user-exists requires --name"
    return 2
fi
if crucible_ssh id -u "$name" >/dev/null 2>&1; then
    return 0
fi
LAST_ERROR="No account named $name on $CRUCIBLE_TARGET_IP"
LAST_STUDENT_MSG="The account '$name' does not exist. Create it with: sudo adduser $name"
return 1
$body$
);

SELECT crucible_seed_action(
    'user-absent',
    'User Account Removed',
    'Asserts a local account has been removed from the student VM.',
    'command', 'system',
    '["linux"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "login name that must not exist"}]'::jsonb,
    '[]'::jsonb,
$body$
local name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$name" ]]; then
    LAST_ERROR="user-absent requires --name"
    return 2
fi
if crucible_ssh id -u "$name" >/dev/null 2>&1; then
    LAST_ERROR="Account $name still exists on $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="The account '$name' still exists. Remove it with: sudo deluser --remove-home $name"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'user-in-group',
    'User Is In Group',
    'Asserts an account is a member of a group — typically used to check who has sudo.',
    'command', 'system',
    '["linux"]'::jsonb,
    '[{"key": "user", "type": "string", "description": "login name"},
      {"key": "group", "type": "string", "description": "group name, e.g. sudo"},
      {"key": "absent", "type": "boolean", "description": "flag with no value; invert the assertion"}]'::jsonb,
    '[]'::jsonb,
$body$
local user="" group="" want_absent=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        --user) user="$2"; shift 2;;
        --group) group="$2"; shift 2;;
        --absent) want_absent=true; shift;;
        *) shift;;
    esac
done
if [[ -z "$user" || -z "$group" ]]; then
    LAST_ERROR="user-in-group requires --user and --group"
    return 2
fi
local groups
groups=$(crucible_ssh id -nG "$user" 2>/dev/null) || groups=""
if [[ -z "$groups" ]]; then
    # An account that does not exist is trivially not in the group, which is
    # exactly what --absent asserts. Failing here would make "prove tempadmin
    # is no longer in sudo" impossible to satisfy by the intended fix of
    # deleting tempadmin.
    if $want_absent; then
        return 0
    fi
    LAST_ERROR="Could not read groups for $user on $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="The account '$user' does not exist, so its group membership could not be checked."
    return 1
fi
# Word-boundary match: a plain substring test would report that a user is in
# "sudo" because they are in "sudoers-readonly".
local is_member=false
if grep -qE "(^| )${group}( |$)" <<<"$groups"; then
    is_member=true
fi
if $want_absent; then
    if $is_member; then
        LAST_ERROR="$user is still in group $group"
        LAST_STUDENT_MSG="The account '$user' should not be in the '$group' group. Remove it with: sudo deluser $user $group"
        return 1
    fi
    return 0
fi
if $is_member; then
    return 0
fi
LAST_ERROR="$user is not in group $group (groups: $groups)"
LAST_STUDENT_MSG="The account '$user' is not in the '$group' group. Add it with: sudo usermod -aG $group $user"
return 1
$body$
);

SELECT crucible_seed_action(
    'account-locked',
    'Account Password Locked',
    'Asserts an account cannot authenticate with a password, by reading its /etc/shadow hash field.',
    'command', 'system',
    '["linux"]'::jsonb,
    '[{"key": "name", "type": "string", "description": "login name"}]'::jsonb,
    '[]'::jsonb,
$body$
local name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$name" ]]; then
    LAST_ERROR="account-locked requires --name"
    return 2
fi
# `passwd -S` is used rather than reading /etc/shadow, so the hash itself never
# crosses the wire or reaches instructor_output. The status letter is L
# (locked), P (usable password) or NP (no password at all).
local status field
status=$(crucible_ssh_sudo passwd -S "$name" 2>&1) || status=""
if [[ -z "$status" || "$status" == *"does not exist"* ]]; then
    LAST_ERROR="Could not read password status for $name on $CRUCIBLE_TARGET_IP: $status"
    LAST_STUDENT_MSG="The account '$name' does not exist, so its password status could not be checked."
    return 1
fi
field=$(awk '{print $2}' <<<"$status")
if [[ "$field" == "L" ]]; then
    return 0
fi
if [[ "$field" == "NP" ]]; then
    LAST_ERROR="Account $name has NO password set at all (status NP)"
    LAST_STUDENT_MSG="The account '$name' has no password at all, which is worse than a weak one. Lock it with: sudo passwd -l $name"
    return 1
fi
LAST_ERROR="Account $name is not locked (passwd -S status: $field)"
LAST_STUDENT_MSG="The account '$name' can still log in with a password. Lock it with: sudo passwd -l $name"
return 1
$body$
);

SELECT crucible_seed_action(
    'no-empty-passwords',
    'No Accounts With Empty Passwords',
    'Asserts no account in /etc/shadow has an empty password field.',
    'command', 'system',
    '["linux"]'::jsonb,
    '[]'::jsonb,
    '[{"key": "empty_password_accounts", "type": "string", "description": "space-separated names, when any were found"}]'::jsonb,
$body$
# Field 2 empty means "authenticates with no password". Only the account NAMES
# are returned; the hashes never leave the target.
local offenders
offenders=$(crucible_ssh_sudo awk -F: '($2 == "") {print $1}' /etc/shadow 2>/dev/null | tr -d '\r' | tr '\n' ' ') || offenders=""
offenders="${offenders%% }"
if [[ -z "$offenders" ]]; then
    return 0
fi
ctx_set "empty_password_accounts" "$offenders"
LAST_ERROR="Accounts with empty passwords on $CRUCIBLE_TARGET_IP: $offenders"
LAST_STUDENT_MSG="These accounts can log in with no password at all: $offenders. Lock or set a password on each one."
return 1
$body$
);

SELECT crucible_seed_action(
    'no-extra-uid-zero',
    'Root Is The Only UID 0',
    'Asserts no account other than root has UID 0 — a classic persistence backdoor.',
    'command', 'system',
    '["linux"]'::jsonb,
    '[]'::jsonb,
    '[{"key": "uid_zero_accounts", "type": "string", "description": "space-separated names of unexpected UID 0 accounts"}]'::jsonb,
$body$
local offenders
offenders=$(crucible_ssh awk -F: '($3 == 0 && $1 != "root") {print $1}' /etc/passwd 2>/dev/null | tr -d '\r' | tr '\n' ' ') || offenders=""
offenders="${offenders%% }"
if [[ -z "$offenders" ]]; then
    return 0
fi
ctx_set "uid_zero_accounts" "$offenders"
LAST_ERROR="Non-root accounts with UID 0 on $CRUCIBLE_TARGET_IP: $offenders"
LAST_STUDENT_MSG="These accounts have root privileges through UID 0: $offenders. Any account with UID 0 is root. Remove them or give them a normal UID."
return 1
$body$
);

SELECT crucible_seed_action(
    'no-passwordless-sudo',
    'No NOPASSWD Sudo Rules',
    'Asserts sudoers grants nobody passwordless root, except the assessment account itself.',
    'command', 'system',
    '["linux"]'::jsonb,
    '[{"key": "allow", "type": "string", "description": "extended regex for entries that are permitted to be NOPASSWD; defaults to the assessment account"}]'::jsonb,
    '[]'::jsonb,
$body$
local allow=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --allow) allow="$2"; shift 2;;
        *) shift;;
    esac
done
# The assessment account itself needs passwordless sudo -- crucible_ssh_sudo
# runs `sudo -n`, so flagging it would make this action assert the absence of
# the very mechanism it uses. Its own entry is exempt by default.
if [[ -z "$allow" ]]; then
    allow="^[[:space:]]*${CRUCIBLE_TARGET_USERNAME:-student}[[:space:]]"
fi
local hits
hits=$(crucible_ssh_sudo grep -rEn 'NOPASSWD' /etc/sudoers /etc/sudoers.d 2>/dev/null | tr -d '\r') || hits=""
if [[ -z "$hits" ]]; then
    return 0
fi
# The grep output is file:line:text, so the allow pattern is matched against
# the rule text only.
local unexpected
unexpected=$(awk -F: '{ $1=""; $2=""; print substr($0,3) }' <<<"$hits" \
    | grep -vE '^[[:space:]]*#' \
    | grep -vE "$allow" \
    | grep -E 'NOPASSWD') || unexpected=""
if [[ -z "$unexpected" ]]; then
    return 0
fi
LAST_ERROR="Unexpected NOPASSWD sudo rules on $CRUCIBLE_TARGET_IP: $unexpected"
LAST_STUDENT_MSG="Sudo is configured to allow root access with no password. Remove the NOPASSWD entries from /etc/sudoers and /etc/sudoers.d/ (edit with visudo)."
return 1
$body$
);

SELECT crucible_seed_action(
    'file-permissions',
    'File Permissions',
    'Asserts a path has an exact octal mode on the student VM.',
    'file_check', 'file',
    '["linux"]'::jsonb,
    '[{"key": "path", "type": "string", "description": "absolute path on the target"},
      {"key": "mode", "type": "string", "description": "expected octal mode, e.g. 600"},
      {"key": "at-most", "type": "boolean", "description": "flag with no value; pass when the mode grants no MORE than --mode"}]'::jsonb,
    '[{"key": "mode_<path>", "type": "string", "description": "the octal mode that was found"}]'::jsonb,
$body$
local path="" mode="" at_most=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        --path) path="$2"; shift 2;;
        --mode) mode="$2"; shift 2;;
        --at-most) at_most=true; shift;;
        *) shift;;
    esac
done
if [[ -z "$path" || -z "$mode" ]]; then
    LAST_ERROR="file-permissions requires --path and --mode"
    return 2
fi
local found
found=$(crucible_ssh_sudo stat -c '%a' "$path" 2>/dev/null | tr -d '\r') || found=""
if [[ -z "$found" ]]; then
    LAST_ERROR="Cannot stat $path on $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="The file $path does not exist."
    return 1
fi
ctx_set "mode_${path//\//_}" "$found"
if [[ "$found" == "$mode" ]]; then
    return 0
fi
if $at_most; then
    # "No more permissive than" is a bitwise question, not a numeric one: 0640
    # is not <= 0600 as an integer, but it grants group read that 0600 does
    # not. Any bit set in `found` that is clear in `mode` is an excess grant.
    local excess=$(( 8#$found & ~8#$mode ))
    if [[ "$excess" -eq 0 ]]; then
        return 0
    fi
    LAST_ERROR="$path is mode $found, which grants more than $mode"
    LAST_STUDENT_MSG="The file $path is mode $found, which is more permissive than $mode. Run: sudo chmod $mode $path"
    return 1
fi
LAST_ERROR="$path is mode $found, expected $mode"
LAST_STUDENT_MSG="The file $path should be mode $mode but is $found. Run: sudo chmod $mode $path"
return 1
$body$
);

SELECT crucible_seed_action(
    'file-owner',
    'File Owner',
    'Asserts a path is owned by an expected user and optionally group.',
    'file_check', 'file',
    '["linux"]'::jsonb,
    '[{"key": "path", "type": "string", "description": "absolute path on the target"},
      {"key": "owner", "type": "string", "description": "expected owning user"},
      {"key": "group", "type": "string", "description": "expected owning group; optional"}]'::jsonb,
    '[]'::jsonb,
$body$
local path="" owner="" group=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --path) path="$2"; shift 2;;
        --owner) owner="$2"; shift 2;;
        --group) group="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$path" || -z "$owner" ]]; then
    LAST_ERROR="file-owner requires --path and --owner"
    return 2
fi
local found
found=$(crucible_ssh_sudo stat -c '%U:%G' "$path" 2>/dev/null | tr -d '\r') || found=""
if [[ -z "$found" ]]; then
    LAST_ERROR="Cannot stat $path on $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="The file $path does not exist."
    return 1
fi
local found_owner="${found%%:*}" found_group="${found##*:}"
if [[ "$found_owner" != "$owner" ]]; then
    LAST_ERROR="$path is owned by $found_owner, expected $owner"
    LAST_STUDENT_MSG="The file $path should be owned by $owner but is owned by $found_owner. Run: sudo chown $owner $path"
    return 1
fi
if [[ -n "$group" && "$found_group" != "$group" ]]; then
    LAST_ERROR="$path has group $found_group, expected $group"
    LAST_STUDENT_MSG="The file $path should have group $group but has $found_group. Run: sudo chgrp $group $path"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'sysctl-value',
    'Kernel Parameter Value',
    'Asserts a running kernel parameter has an expected value.',
    'command', 'system',
    '["linux"]'::jsonb,
    '[{"key": "key", "type": "string", "description": "sysctl key, e.g. net.ipv4.conf.all.rp_filter"},
      {"key": "value", "type": "string", "description": "expected value"}]'::jsonb,
    '[{"key": "sysctl_<key>", "type": "string", "description": "the running value that was found"}]'::jsonb,
$body$
local key="" value=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --key) key="$2"; shift 2;;
        --value) value="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$key" || -z "$value" ]]; then
    LAST_ERROR="sysctl-value requires --key and --value"
    return 2
fi
# The RUNNING value, not the file: a setting written to /etc/sysctl.conf and
# never applied is the most common way this check is failed while looking
# correct on disk.
local found
found=$(crucible_ssh sysctl -n "$key" 2>/dev/null | tr -d '\r' | tr -s '[:space:]' ' ') || found=""
found="${found%% }"
if [[ -z "$found" ]]; then
    LAST_ERROR="sysctl key $key does not exist on $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="The kernel setting $key does not exist on this system."
    return 1
fi
ctx_set "sysctl_${key//./_}" "$found"
if [[ "$found" == "$value" ]]; then
    return 0
fi
LAST_ERROR="sysctl $key is '$found', expected '$value'"
LAST_STUDENT_MSG="Kernel setting $key is '$found' but should be '$value'. Set it in /etc/sysctl.d/ and apply with: sudo sysctl --system"
return 1
$body$
);

SELECT crucible_seed_action(
    'no-world-writable',
    'No World-Writable Files',
    'Asserts a directory tree contains no world-writable regular files (sticky-bit directories excluded).',
    'file_check', 'file',
    '["linux"]'::jsonb,
    '[{"key": "path", "type": "string", "description": "directory to scan; defaults to /etc"},
      {"key": "max-depth", "type": "string", "description": "how deep to descend; defaults to 4"}]'::jsonb,
    '[{"key": "world_writable_files", "type": "string", "description": "space-separated offending paths"}]'::jsonb,
$body$
local path="/etc" depth="4"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --path) path="$2"; shift 2;;
        --max-depth) depth="$2"; shift 2;;
        *) shift;;
    esac
done
# -type f only: /tmp and /var/tmp are legitimately world-writable directories,
# and flagging them would make this action fail on every stock system.
# The result is capped so one badly-permissioned tree cannot produce a
# megabyte of instructor output.
local offenders
offenders=$(crucible_ssh_sudo find "$path" -maxdepth "$depth" -type f -perm -0002 -print 2>/dev/null \
    | head -20 | tr -d '\r' | tr '\n' ' ') || offenders=""
offenders="${offenders%% }"
if [[ -z "$offenders" ]]; then
    return 0
fi
ctx_set "world_writable_files" "$offenders"
LAST_ERROR="World-writable files under $path on $CRUCIBLE_TARGET_IP: $offenders"
LAST_STUDENT_MSG="These files can be modified by any user on the system: $offenders. Remove world-write with: sudo chmod o-w <file>"
return 1
$body$
);

SELECT crucible_seed_action(
    'cron-entry-absent',
    'No Matching Cron Entry',
    'Asserts no crontab on the target contains text matching a regex — used to find scheduled persistence.',
    'command', 'system',
    '["linux"]'::jsonb,
    '[{"key": "pattern", "type": "string", "description": "extended regex matched against every crontab line"}]'::jsonb,
    '[]'::jsonb,
$body$
local pattern=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --pattern) pattern="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$pattern" ]]; then
    LAST_ERROR="cron-entry-absent requires --pattern"
    return 2
fi
# Both the system crontabs and per-user spools; a backdoor lives in whichever
# one the author did not think to check.
local hits
hits=$(crucible_ssh_sudo grep -rEl -- "$pattern" /etc/crontab /etc/cron.d /var/spool/cron 2>/dev/null \
    | head -10 | tr -d '\r' | tr '\n' ' ') || hits=""
hits="${hits%% }"
if [[ -z "$hits" ]]; then
    return 0
fi
LAST_ERROR="Cron entries matching '$pattern' found in: $hits"
LAST_STUDENT_MSG="A scheduled task matching '$pattern' is still present in: $hits. Remove it."
return 1
$body$
);

SELECT crucible_seed_action(
    'unattended-upgrades-enabled',
    'Automatic Security Updates Enabled',
    'Asserts unattended-upgrades is installed and configured to run.',
    'service_check', 'service',
    '["linux:ubuntu", "linux:debian"]'::jsonb,
    '[]'::jsonb,
    '[]'::jsonb,
$body$
local status
status=$(crucible_ssh dpkg-query -W -f='${Status}' unattended-upgrades 2>/dev/null) || status=""
if [[ "$status" != *"install ok installed"* ]]; then
    LAST_ERROR="unattended-upgrades is not installed on $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="Automatic security updates are not installed. Run: sudo apt install unattended-upgrades"
    return 1
fi
# The package can be installed while the periodic timer is set to 0, which
# looks configured and updates nothing.
local period
period=$(crucible_ssh_sudo apt-config dump APT::Periodic::Unattended-Upgrade 2>/dev/null | tr -d '\r"') || period=""
if [[ "$period" == *"Unattended-Upgrade 0"* || -z "$period" ]]; then
    LAST_ERROR="unattended-upgrades is installed but APT::Periodic::Unattended-Upgrade is not enabled ($period)"
    LAST_STUDENT_MSG="Automatic security updates are installed but switched off. Run: sudo dpkg-reconfigure -plow unattended-upgrades"
    return 1
fi
return 0
$body$
);


-- ===========================================================================
-- RECON — observed from the runner, over the pod VLAN
-- ===========================================================================

SELECT crucible_seed_action(
    'nmap-port-state',
    'Nmap Reports Port State',
    'Asserts nmap sees a port in an expected state (open, closed, or filtered).',
    'port_check', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "port", "type": "string", "description": "single TCP port"},
      {"key": "state", "type": "string", "description": "open, closed or filtered; defaults to open"}]'::jsonb,
    '[{"key": "nmap_port_<port>_state", "type": "string", "description": "the state nmap reported"}]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" port="" state="open"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        --state) state="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" || -z "$port" ]]; then
    LAST_ERROR="nmap-port-state requires --port, and --host or CRUCIBLE_TARGET_IP"
    return 2
fi
# -Pn because a hardened target that drops ICMP would otherwise be reported as
# "host down" and every port scan against it would return nothing at all --
# indistinguishable from a closed port, and wrong.
local out found
out=$(nmap -Pn -p "$port" --host-timeout 20s "$host" 2>&1) || true
found=$(grep -E "^${port}/tcp" <<<"$out" | awk '{print $2}' | head -1)
ctx_set "nmap_port_${port}_state" "${found:-unknown}"
# nmap reports a port that answers neither way as "filtered"; a firewall that
# rejects rather than drops shows "closed". Both mean "not reachable", so a
# request for either accepts the other.
if [[ "$found" == "$state" ]]; then
    return 0
fi
if [[ "$state" != "open" && "$found" != "open" && -n "$found" ]]; then
    return 0
fi
LAST_ERROR="nmap reports port $port on $host as '${found:-no result}', expected '$state'"
if [[ "$state" == "open" ]]; then
    LAST_STUDENT_MSG="Port $port is not open on $host (nmap says '${found:-no result}'). Make sure the service is listening and the firewall allows it."
else
    LAST_STUDENT_MSG="Port $port is still open on $host. Stop the service or block the port with your firewall."
fi
return 1
$body$
);

SELECT crucible_seed_action(
    'nmap-only-expected-ports',
    'No Unexpected Open Ports',
    'Scans a port range and asserts nothing outside an allowlist is open — the check that catches a service the student forgot to disable.',
    'port_check', 'firewall',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "allow", "type": "string", "description": "comma-separated ports that may be open, e.g. 22,80,443"},
      {"key": "range", "type": "string", "description": "nmap port spec to scan; defaults to 1-1024"}]'::jsonb,
    '[{"key": "unexpected_open_ports", "type": "string", "description": "comma-separated ports that were open but not allowed"}]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" allow="" range="1-1024"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --allow) allow="$2"; shift 2;;
        --range) range="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="nmap-only-expected-ports requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
local out open unexpected=""
out=$(nmap -Pn -p "$range" --open --host-timeout 60s "$host" 2>&1) || true
# A scan that produced no port table at all is a failed scan, not a clean
# host. Reporting "no unexpected ports" for it would be a false pass on the
# most important check in this file.
if ! grep -qE '^PORT[[:space:]]+STATE' <<<"$out"; then
    LAST_ERROR="nmap produced no port table for $host: $out"
    LAST_STUDENT_MSG="This check could not scan $host. Please report it to your instructor."
    return 2
fi
open=$(grep -E '^[0-9]+/tcp[[:space:]]+open' <<<"$out" | cut -d/ -f1)
local port
for port in $open; do
    if ! grep -qE "(^|,)${port}(,|$)" <<<"$allow"; then
        unexpected="${unexpected}${unexpected:+,}${port}"
    fi
done
if [[ -z "$unexpected" ]]; then
    return 0
fi
ctx_set "unexpected_open_ports" "$unexpected"
LAST_ERROR="Unexpected open ports on $host: $unexpected (allowed: ${allow:-none})"
LAST_STUDENT_MSG="These ports are open but should not be: $unexpected. Stop the services listening on them, or block them with your firewall."
return 1
$body$
);

SELECT crucible_seed_action(
    'nmap-script-output',
    'Nmap Script Output Matches',
    'Runs a named nmap NSE script against a port and asserts its output matches a regex.',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "port", "type": "string", "description": "TCP port"},
      {"key": "script", "type": "string", "description": "NSE script name, e.g. ssl-enum-ciphers"},
      {"key": "expect", "type": "string", "description": "extended regex the output must match"},
      {"key": "absent", "type": "boolean", "description": "flag with no value; invert, so --expect must NOT appear"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" port="" script="" expect="" want_absent=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        --script) script="$2"; shift 2;;
        --expect) expect="$2"; shift 2;;
        --absent) want_absent=true; shift;;
        *) shift;;
    esac
done
if [[ -z "$host" || -z "$port" || -z "$script" || -z "$expect" ]]; then
    LAST_ERROR="nmap-script-output requires --port, --script, --expect, and --host or CRUCIBLE_TARGET_IP"
    return 2
fi
local out
out=$(nmap -Pn -p "$port" --script "$script" --host-timeout 25s "$host" 2>&1) || true
if ! grep -qE "^${port}/tcp[[:space:]]+open" <<<"$out"; then
    LAST_ERROR="Port $port on $host is not open, so $script produced nothing"
    LAST_STUDENT_MSG="Port $port is not open on $host, so this check could not run. Start the service first."
    return 1
fi
if grep -qE -- "$expect" <<<"$out"; then
    if $want_absent; then
        LAST_ERROR="$script output still matches '$expect' on $host:$port"
        LAST_STUDENT_MSG="The service on port $port still reports '$expect'. Reconfigure it."
        return 1
    fi
    return 0
fi
if $want_absent; then
    return 0
fi
LAST_ERROR="$script output does not match '$expect' on $host:$port"
LAST_STUDENT_MSG="The service on port $port does not report '$expect' as expected."
return 1
$body$
);

SELECT crucible_seed_action(
    'host-responds-to-ping',
    'Host Responds To Ping',
    'Asserts the target answers ICMP echo — or, with --absent, that it deliberately does not.',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "absent", "type": "boolean", "description": "flag with no value; assert the host does NOT answer"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" want_absent=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --absent) want_absent=true; shift;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="host-responds-to-ping requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
local alive=false
if ping -c 2 -W 3 "$host" >/dev/null 2>&1; then
    alive=true
fi
if $want_absent; then
    if $alive; then
        LAST_ERROR="$host still answers ICMP echo"
        LAST_STUDENT_MSG="Your host still responds to ping. Block ICMP echo requests if the task asks you to."
        return 1
    fi
    return 0
fi
if $alive; then
    return 0
fi
LAST_ERROR="$host does not answer ICMP echo"
LAST_STUDENT_MSG="Your host is not reachable on the network. Check that it is powered on and its network interface is up."
return 1
$body$
);

SELECT crucible_seed_action(
    'smb-share-listable',
    'SMB Shares Are Enumerable',
    'Asserts (or denies) that SMB shares can be listed anonymously — a null-session check.',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "expect-share", "type": "string", "description": "share name that must appear in the listing"},
      {"key": "absent", "type": "boolean", "description": "flag with no value; assert anonymous listing is REFUSED"}]'::jsonb,
    '[{"key": "smb_shares", "type": "string", "description": "space-separated share names that were listed"}]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" expect_share="" want_absent=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --expect-share) expect_share="$2"; shift 2;;
        --absent) want_absent=true; shift;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="smb-share-listable requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
local out rc=0
out=$(smbclient -L "//${host}" -N -t 10 2>&1) || rc=$?
local shares
shares=$(grep -E '^[[:space:]]+[A-Za-z0-9_$.-]+[[:space:]]+(Disk|IPC|Printer)' <<<"$out" \
    | awk '{print $1}' | tr '\n' ' ')
shares="${shares%% }"
ctx_set "smb_shares" "$shares"

if $want_absent; then
    if [[ $rc -eq 0 && -n "$shares" ]]; then
        LAST_ERROR="Anonymous SMB listing still works on $host: $shares"
        LAST_STUDENT_MSG="Anyone can list your SMB shares without credentials ($shares). Disable guest access in smb.conf."
        return 1
    fi
    return 0
fi
if [[ $rc -ne 0 ]]; then
    LAST_ERROR="smbclient could not list shares on $host: $out"
    LAST_STUDENT_MSG="Could not list SMB shares on $host. Is Samba running and reachable on port 445?"
    return 1
fi
if [[ -n "$expect_share" ]] && ! grep -qE "(^| )${expect_share}( |$)" <<<"$shares"; then
    LAST_ERROR="Share '$expect_share' not present on $host (found: $shares)"
    LAST_STUDENT_MSG="The SMB share '$expect_share' does not exist. Shares found: ${shares:-none}"
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'ldap-anonymous-bind-denied',
    'LDAP Anonymous Bind Denied',
    'Asserts the directory refuses to return entries to an unauthenticated client.',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "base", "type": "string", "description": "search base DN"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" base=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --base) base="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" || -z "$base" ]]; then
    LAST_ERROR="ldap-anonymous-bind-denied requires --base, and --host or CRUCIBLE_TARGET_IP"
    return 2
fi
local out
out=$(ldapsearch -x -H "ldap://${host}" -b "$base" -o nettimeout=10 -LLL '(objectClass=*)' dn 2>&1) || true
if grep -qE '^dn:' <<<"$out"; then
    LAST_ERROR="Anonymous LDAP bind returned entries from $host"
    LAST_STUDENT_MSG="Your directory returns entries to anyone without a password. Disable anonymous bind."
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'redis-auth-required',
    'Redis Requires Authentication',
    'Asserts a Redis instance rejects unauthenticated commands.',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "port", "type": "string", "description": "Redis port; defaults to 6379"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" port="6379"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="redis-auth-required requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
# A Redis that is not listening at all trivially "requires auth". That is a
# different outcome from a protected one and must not be reported as a pass.
if ! nc -zw3 "$host" "$port" 2>/dev/null; then
    LAST_ERROR="Nothing is listening on ${host}:${port}"
    LAST_STUDENT_MSG="No Redis service is reachable on ${host}:${port}, so its authentication could not be checked."
    return 1
fi
local out
out=$(redis-cli -h "$host" -p "$port" --timeout 5 INFO server 2>&1) || true
if grep -qiE 'NOAUTH|authentication required|operation not permitted' <<<"$out"; then
    return 0
fi
if grep -qi 'redis_version' <<<"$out"; then
    LAST_ERROR="Redis on ${host}:${port} answered INFO without authentication"
    LAST_STUDENT_MSG="Your Redis server answers commands from anyone. Set a password with the requirepass directive in redis.conf."
    return 1
fi
LAST_ERROR="Unexpected reply from Redis on ${host}:${port}: $out"
LAST_STUDENT_MSG="Could not determine whether Redis requires a password on ${host}:${port}."
return 1
$body$
);


-- ===========================================================================
-- WEB — TLS and HTTP posture, observed from the runner
-- ===========================================================================

SELECT crucible_seed_action(
    'tls-certificate-valid',
    'TLS Certificate Is Currently Valid',
    'Asserts the served certificate is inside its validity window, and optionally not expiring within N days.',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "port", "type": "string", "description": "TLS port; defaults to 443"},
      {"key": "days", "type": "string", "description": "fail if the certificate expires within this many days"}]'::jsonb,
    '[{"key": "tls_not_after", "type": "string", "description": "the certificate expiry date"}]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" port="443" days=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        --days) days="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="tls-certificate-valid requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
# </dev/null so s_client does not sit waiting for stdin until the action times
# out; -servername so a name-based virtual host serves the right certificate.
local pem
pem=$(openssl s_client -connect "${host}:${port}" -servername "$host" </dev/null 2>/dev/null \
    | openssl x509 2>/dev/null) || pem=""
if [[ -z "$pem" ]]; then
    LAST_ERROR="No TLS certificate served on ${host}:${port}"
    LAST_STUDENT_MSG="No TLS certificate was returned by ${host}:${port}. Is HTTPS configured and running?"
    return 1
fi
local not_after
not_after=$(openssl x509 -noout -enddate <<<"$pem" 2>/dev/null | cut -d= -f2-)
ctx_set "tls_not_after" "$not_after"

if ! openssl x509 -noout -checkend 0 <<<"$pem" >/dev/null 2>&1; then
    LAST_ERROR="Certificate on ${host}:${port} expired at $not_after"
    LAST_STUDENT_MSG="Your TLS certificate expired on $not_after. Renew or reissue it."
    return 1
fi
if [[ -n "$days" ]]; then
    local seconds=$(( days * 86400 ))
    if ! openssl x509 -noout -checkend "$seconds" <<<"$pem" >/dev/null 2>&1; then
        LAST_ERROR="Certificate on ${host}:${port} expires within $days days (at $not_after)"
        LAST_STUDENT_MSG="Your TLS certificate expires on $not_after, which is less than $days days away. Renew it."
        return 1
    fi
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'tls-protocol-refused',
    'Obsolete TLS Protocol Refused',
    'Asserts the server refuses a specific TLS or SSL version.',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "port", "type": "string", "description": "TLS port; defaults to 443"},
      {"key": "protocol", "type": "string", "description": "one of tls1, tls1_1, tls1_2, ssl3"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" port="443" protocol="tls1_1"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        --protocol) protocol="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="tls-protocol-refused requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
# The allowlist is a quoted regex rather than a `case tls1|ssl3)` arm on
# purpose: the authoring-time command checker reads a bare word after `|` as a
# new command, and would report this action as depending on a `tls1` binary.
if ! grep -qE '^(tls1|tls1_1|tls1_2|tls1_3|ssl3)$' <<<"$protocol"; then
    LAST_ERROR="tls-protocol-refused: unsupported --protocol '$protocol'"
    return 2
fi
# A version this openssl build refuses to even offer would look identical to a
# server refusing it, which would be a false pass. The handshake output is
# checked for a server-side alert rather than trusting the exit code alone.
local out
out=$(openssl s_client -connect "${host}:${port}" -servername "$host" "-${protocol}" </dev/null 2>&1) || true
if grep -qiE 'unknown option|unsupported protocol.*not (supported|compiled)' <<<"$out"; then
    LAST_ERROR="This runner's openssl cannot offer $protocol, so refusal could not be proven"
    LAST_STUDENT_MSG="This check could not test $protocol. Please report it to your instructor."
    return 2
fi
if grep -qE 'Cipher[[:space:]]*:[[:space:]]*\(NONE\)|no peer certificate|handshake failure|alert protocol version|wrong version number' <<<"$out"; then
    return 0
fi
if grep -qE 'Verify return code|Cipher[[:space:]]*:' <<<"$out"; then
    LAST_ERROR="Server on ${host}:${port} completed a $protocol handshake"
    LAST_STUDENT_MSG="Your web server still accepts $protocol, which is obsolete. Restrict it to TLS 1.2 and above."
    return 1
fi
LAST_ERROR="Could not reach ${host}:${port} to test $protocol: $out"
LAST_STUDENT_MSG="Could not connect to ${host}:${port} over TLS. Is HTTPS running?"
return 1
$body$
);

SELECT crucible_seed_action(
    'http-header-present',
    'HTTP Response Header Present',
    'Asserts a response header exists, optionally with a value matching a regex.',
    'http', 'network',
    '["any"]'::jsonb,
    '[{"key": "url", "type": "string", "description": "full URL to request"},
      {"key": "header", "type": "string", "description": "header name, matched case-insensitively"},
      {"key": "value", "type": "string", "description": "extended regex the header value must match; optional"}]'::jsonb,
    '[{"key": "header_<name>", "type": "string", "description": "the header value that was found"}]'::jsonb,
$body$
local url="" header="" value=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url) url="$2"; shift 2;;
        --header) header="$2"; shift 2;;
        --value) value="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$url" || -z "$header" ]]; then
    LAST_ERROR="http-header-present requires --url and --header"
    return 2
fi
# -k because a student's lab TLS certificate is self-signed by definition and
# rejecting it would make every header check fail for the wrong reason.
local headers found
headers=$(curl -sS -k -I -L --max-time 10 "$url" 2>&1) || headers=""
if [[ -z "$headers" ]]; then
    LAST_ERROR="No response from $url"
    LAST_STUDENT_MSG="No response from $url. Is the web server running?"
    return 1
fi
found=$(grep -iE "^${header}:" <<<"$headers" | tail -1 | cut -d: -f2- | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
ctx_set "header_${header,,}" "$found"
if [[ -z "$found" ]]; then
    LAST_ERROR="$url does not send a $header header"
    LAST_STUDENT_MSG="The response from $url is missing the $header header. Add it to your web server configuration."
    return 1
fi
if [[ -n "$value" ]] && ! grep -qE -- "$value" <<<"$found"; then
    LAST_ERROR="$header on $url is '$found', which does not match '$value'"
    LAST_STUDENT_MSG="The $header header is '$found' but should match '$value'."
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'http-header-absent',
    'HTTP Response Header Absent',
    'Asserts a response header is not sent — used for version-leaking headers such as Server and X-Powered-By.',
    'http', 'network',
    '["any"]'::jsonb,
    '[{"key": "url", "type": "string", "description": "full URL to request"},
      {"key": "header", "type": "string", "description": "header name, matched case-insensitively"},
      {"key": "value", "type": "string", "description": "only fail when the value matches this regex; optional"}]'::jsonb,
    '[]'::jsonb,
$body$
local url="" header="" value=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url) url="$2"; shift 2;;
        --header) header="$2"; shift 2;;
        --value) value="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$url" || -z "$header" ]]; then
    LAST_ERROR="http-header-absent requires --url and --header"
    return 2
fi
local headers found
headers=$(curl -sS -k -I -L --max-time 10 "$url" 2>&1) || headers=""
if [[ -z "$headers" ]]; then
    LAST_ERROR="No response from $url"
    LAST_STUDENT_MSG="No response from $url. Is the web server running?"
    return 1
fi
found=$(grep -iE "^${header}:" <<<"$headers" | tail -1 | cut -d: -f2- | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
if [[ -z "$found" ]]; then
    return 0
fi
if [[ -n "$value" ]] && ! grep -qE -- "$value" <<<"$found"; then
    return 0
fi
LAST_ERROR="$url still sends $header: $found"
LAST_STUDENT_MSG="Your web server still sends the $header header ('$found'), which tells an attacker what software and version you run. Suppress it in your server configuration."
return 1
$body$
);

SELECT crucible_seed_action(
    'http-redirects-to-https',
    'HTTP Redirects To HTTPS',
    'Asserts a plain HTTP request is answered with a redirect to an https:// URL.',
    'http', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "path", "type": "string", "description": "path to request; defaults to /"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" path="/"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --path) path="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="http-redirects-to-https requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
# No -L: following the redirect would hide whether one happened at all.
local headers status location
headers=$(curl -sS -k -I --max-time 10 "http://${host}${path}" 2>&1) || headers=""
if [[ -z "$headers" ]]; then
    LAST_ERROR="No response from http://${host}${path}"
    LAST_STUDENT_MSG="No response on plain HTTP from $host. Is the web server running on port 80?"
    return 1
fi
status=$(grep -oE '^HTTP/[0-9.]+ [0-9]{3}' <<<"$headers" | tail -1 | awk '{print $2}')
location=$(grep -iE '^location:' <<<"$headers" | tail -1 | cut -d: -f2- | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
if [[ "$status" =~ ^30[1278]$ ]] && [[ "$location" == https://* ]]; then
    return 0
fi
LAST_ERROR="http://${host}${path} answered $status with Location '${location:-none}'"
LAST_STUDENT_MSG="Plain HTTP requests are not redirected to HTTPS (got status ${status:-none}). Configure your web server to redirect port 80 to https://."
return 1
$body$
);

SELECT crucible_seed_action(
    'directory-listing-disabled',
    'Directory Listing Disabled',
    'Asserts a URL does not return an auto-generated index of its directory.',
    'http', 'web',
    '["any"]'::jsonb,
    '[{"key": "url", "type": "string", "description": "directory URL to request, e.g. http://host/uploads/"}]'::jsonb,
    '[]'::jsonb,
$body$
local url=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url) url="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$url" ]]; then
    LAST_ERROR="directory-listing-disabled requires --url"
    return 2
fi
local body status
status=$(curl -sS -k -L --max-time 10 -o /dev/null -w '%{http_code}' "$url" 2>/dev/null) || status="000"
if [[ "$status" == "000" ]]; then
    LAST_ERROR="No response from $url"
    LAST_STUDENT_MSG="No response from $url. Is the web server running?"
    return 1
fi
# 403 or 404 is the desired outcome: the server is refusing rather than
# enumerating.
if [[ "$status" == "403" || "$status" == "404" ]]; then
    return 0
fi
body=$(curl -sS -k -L --max-time 10 "$url" 2>/dev/null | head -c 8192) || body=""
# The three stock auto-index banners: Apache, nginx and lighttpd.
if grep -qiE '<title>Index of |<h1>Index of |Directory listing for ' <<<"$body"; then
    LAST_ERROR="$url returns an auto-generated directory index"
    LAST_STUDENT_MSG="Browsing $url lists the directory's contents. Turn off automatic indexing (Apache: Options -Indexes; nginx: autoindex off)."
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'http-auth-required',
    'HTTP Endpoint Requires Authentication',
    'Asserts an unauthenticated request to a URL is refused with 401 or 403.',
    'http', 'web',
    '["any"]'::jsonb,
    '[{"key": "url", "type": "string", "description": "full URL to request"}]'::jsonb,
    '[{"key": "http_auth_status", "type": "string", "description": "the status code returned"}]'::jsonb,
$body$
local url=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url) url="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$url" ]]; then
    LAST_ERROR="http-auth-required requires --url"
    return 2
fi
# No -L: a 302 to a login page is a redirect, not a refusal, and following it
# would turn the login page's own 200 into a false pass.
local status
status=$(curl -sS -k --max-time 10 -o /dev/null -w '%{http_code}' "$url" 2>/dev/null) || status="000"
ctx_set "http_auth_status" "$status"
if [[ "$status" == "401" || "$status" == "403" ]]; then
    return 0
fi
if [[ "$status" == "000" ]]; then
    LAST_ERROR="No response from $url"
    LAST_STUDENT_MSG="No response from $url. Is the web server running?"
    return 1
fi
LAST_ERROR="$url returned $status to an unauthenticated request, expected 401 or 403"
LAST_STUDENT_MSG="Anyone can reach $url without logging in (it returned $status). Require authentication for this path."
return 1
$body$
);

SELECT crucible_seed_action(
    'http-credentials-rejected',
    'Default Credentials Rejected',
    'Asserts a username and password pair is NOT accepted — for proving default credentials were changed.',
    'http', 'web',
    '["any"]'::jsonb,
    '[{"key": "url", "type": "string", "description": "full URL protected by HTTP basic auth"},
      {"key": "user", "type": "string", "description": "username that must be rejected"},
      {"key": "pass", "type": "string", "description": "password that must be rejected"}]'::jsonb,
    '[]'::jsonb,
$body$
local url="" user="" pass=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url) url="$2"; shift 2;;
        --user) user="$2"; shift 2;;
        --pass) pass="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$url" || -z "$user" ]]; then
    LAST_ERROR="http-credentials-rejected requires --url and --user"
    return 2
fi
local status
status=$(curl -sS -k --max-time 10 -o /dev/null -w '%{http_code}' -u "${user}:${pass}" "$url" 2>/dev/null) || status="000"
if [[ "$status" == "000" ]]; then
    LAST_ERROR="No response from $url"
    LAST_STUDENT_MSG="No response from $url. Is the web server running?"
    return 1
fi
if [[ "$status" == "401" || "$status" == "403" ]]; then
    return 0
fi
# The password is deliberately not echoed into instructor_output.
LAST_ERROR="$url accepted user '$user' with the default password (status $status)"
LAST_STUDENT_MSG="The default credentials for '$user' still work on $url. Change that account's password."
return 1
$body$
);

SELECT crucible_seed_action(
    'web-technology-hidden',
    'Web Server Version Not Advertised',
    'Uses whatweb to assert the site does not advertise a specific software version string.',
    'http', 'web',
    '["any"]'::jsonb,
    '[{"key": "url", "type": "string", "description": "full URL to fingerprint"},
      {"key": "deny", "type": "string", "description": "extended regex that must NOT appear in the fingerprint; defaults to a version-number pattern"}]'::jsonb,
    '[{"key": "whatweb_summary", "type": "string", "description": "the one-line fingerprint whatweb produced"}]'::jsonb,
$body$
local url="" deny=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --url) url="$2"; shift 2;;
        --deny) deny="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$url" ]]; then
    LAST_ERROR="web-technology-hidden requires --url"
    return 2
fi
# whatweb reports e.g. `Apache[2.4.52]`. The default pattern is that shape --
# a bracketed dotted version after a product name -- rather than any digit,
# which would match a page's own content.
if [[ -z "$deny" ]]; then
    deny='\[[0-9]+\.[0-9]+(\.[0-9]+)?\]'
fi
local out
out=$(whatweb --color=never --no-errors -a 1 --open-timeout 10 --read-timeout 10 "$url" 2>&1 | head -c 4000) || out=""
if [[ -z "$out" ]]; then
    LAST_ERROR="whatweb returned nothing for $url"
    LAST_STUDENT_MSG="Could not fingerprint $url. Is the web server running?"
    return 1
fi
ctx_set "whatweb_summary" "$out"
if grep -qE -- "$deny" <<<"$out"; then
    LAST_ERROR="Fingerprint for $url still advertises a version: $out"
    LAST_STUDENT_MSG="Your web server advertises its exact version, which tells an attacker which exploits to try. Suppress version numbers in your server configuration (Apache: ServerTokens Prod; nginx: server_tokens off)."
    return 1
fi
return 0
$body$
);


-- ===========================================================================
-- SERVICES — reachability and banner checks from the runner
-- ===========================================================================

SELECT crucible_seed_action(
    'tcp-banner-matches',
    'TCP Banner Matches',
    'Connects to a port, reads whatever the service announces, and matches it against a regex.',
    'port_check', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "port", "type": "string", "description": "TCP port"},
      {"key": "expect", "type": "string", "description": "extended regex the banner must match"},
      {"key": "absent", "type": "boolean", "description": "flag with no value; invert, so --expect must NOT appear"}]'::jsonb,
    '[{"key": "banner_<port>", "type": "string", "description": "the banner text that was read"}]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" port="" expect="" want_absent=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        --expect) expect="$2"; shift 2;;
        --absent) want_absent=true; shift;;
        *) shift;;
    esac
done
if [[ -z "$host" || -z "$port" || -z "$expect" ]]; then
    LAST_ERROR="tcp-banner-matches requires --port, --expect, and --host or CRUCIBLE_TARGET_IP"
    return 2
fi
# -w 5 bounds the read: a service that connects and says nothing would
# otherwise hold the socket until the action's own timeout kills it, which
# reports as a timeout rather than as an empty banner.
local banner
banner=$(nc -w 5 "$host" "$port" </dev/null 2>/dev/null | head -c 512 | tr -d '\r') || banner=""
ctx_set "banner_${port}" "$banner"
if [[ -z "$banner" ]]; then
    LAST_ERROR="No banner from ${host}:${port}"
    LAST_STUDENT_MSG="Nothing is listening on port $port, or the service sends no banner."
    return 1
fi
if grep -qE -- "$expect" <<<"$banner"; then
    if $want_absent; then
        LAST_ERROR="Banner on ${host}:${port} still matches '$expect': $banner"
        LAST_STUDENT_MSG="The service on port $port still announces '$expect'. Change its banner."
        return 1
    fi
    return 0
fi
if $want_absent; then
    return 0
fi
LAST_ERROR="Banner on ${host}:${port} does not match '$expect': $banner"
LAST_STUDENT_MSG="The service on port $port announced '$banner', which does not match the expected '$expect'."
return 1
$body$
);

SELECT crucible_seed_action(
    'service-listening-on',
    'Service Listens On Expected Address',
    'Asserts a port is bound to an expected address on the target — the check that catches a database listening on 0.0.0.0 instead of localhost.',
    'command', 'network',
    '["linux"]'::jsonb,
    '[{"key": "port", "type": "string", "description": "TCP port"},
      {"key": "address", "type": "string", "description": "expected bind address, e.g. 127.0.0.1"}]'::jsonb,
    '[{"key": "listen_<port>", "type": "string", "description": "the address:port that was found bound"}]'::jsonb,
$body$
local port="" address=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --port) port="$2"; shift 2;;
        --address) address="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$port" || -z "$address" ]]; then
    LAST_ERROR="service-listening-on requires --port and --address"
    return 2
fi
# `ss -H -ltn` gives one socket per line with no header. This is read on the
# TARGET, because "which interface is it bound to" is not answerable from the
# network: a socket on 0.0.0.0 and one on the VLAN address look identical to a
# port scan.
local sockets found
sockets=$(crucible_ssh ss -H -ltn 2>/dev/null | tr -d '\r') || sockets=""
if [[ -z "$sockets" ]]; then
    LAST_ERROR="Could not list listening sockets on $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="This check could not read the listening sockets on your VM."
    return 1
fi
found=$(awk -v p=":$port" '$4 ~ p"$" {print $4}' <<<"$sockets" | head -1)
ctx_set "listen_${port}" "${found:-none}"
if [[ -z "$found" ]]; then
    LAST_ERROR="Nothing is listening on port $port on $CRUCIBLE_TARGET_IP"
    LAST_STUDENT_MSG="No service is listening on port $port. Start the service first."
    return 1
fi
if [[ "$found" == "${address}:${port}" || "$found" == "[${address}]:${port}" ]]; then
    return 0
fi
LAST_ERROR="Port $port is bound to $found, expected ${address}:${port}"
LAST_STUDENT_MSG="The service on port $port is listening on $found instead of ${address}:${port}. Change its bind address in its configuration file."
return 1
$body$
);

SELECT crucible_seed_action(
    'ftp-anonymous-denied',
    'Anonymous FTP Denied',
    'Asserts the FTP service refuses the anonymous account.',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "port", "type": "string", "description": "FTP port; defaults to 21"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" port="21"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="ftp-anonymous-denied requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
if ! nc -zw3 "$host" "$port" 2>/dev/null; then
    # No FTP service is the strongest possible form of "anonymous FTP is
    # denied", so this is a pass, but it is worth saying which one happened.
    ctx_set "ftp_state" "closed"
    return 0
fi
# nmap's ftp-anon script does the login dance correctly, including the PASV
# negotiation a hand-rolled nc conversation gets wrong.
local out
out=$(nmap -Pn -p "$port" --script ftp-anon --host-timeout 20s "$host" 2>&1) || true
if grep -qi 'Anonymous FTP login allowed' <<<"$out"; then
    LAST_ERROR="Anonymous FTP login is allowed on ${host}:${port}"
    LAST_STUDENT_MSG="Anyone can log into your FTP server as 'anonymous'. Disable anonymous access in the FTP server configuration."
    return 1
fi
return 0
$body$
);

SELECT crucible_seed_action(
    'smb-signing-required',
    'SMB Signing Required',
    'Asserts the SMB service requires message signing, which blocks relay attacks.',
    'command', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "target host, defaults to CRUCIBLE_TARGET_IP"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="smb-signing-required requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
if ! nc -zw3 "$host" 445 2>/dev/null; then
    LAST_ERROR="Nothing is listening on ${host}:445"
    LAST_STUDENT_MSG="No SMB service is reachable on $host, so its signing policy could not be checked."
    return 1
fi
local out
out=$(nmap -Pn -p 445 --script smb2-security-mode --host-timeout 25s "$host" 2>&1) || true
if grep -qi 'Message signing enabled and required' <<<"$out"; then
    return 0
fi
if grep -qi 'Message signing enabled but not required\|Message signing disabled' <<<"$out"; then
    LAST_ERROR="SMB signing is not required on $host"
    LAST_STUDENT_MSG="Your SMB server does not require message signing, which allows relay attacks. Set 'server signing = mandatory' in smb.conf."
    return 1
fi
LAST_ERROR="Could not determine SMB signing policy on $host: $out"
LAST_STUDENT_MSG="Could not determine your SMB signing policy. Is Samba running?"
return 1
$body$
);

SELECT crucible_seed_action(
    'dns-server-refuses-recursion',
    'DNS Server Refuses Open Recursion',
    'Asserts a DNS service will not resolve arbitrary external names for any client.',
    'dns', 'network',
    '["any"]'::jsonb,
    '[{"key": "host", "type": "string", "description": "DNS server to query, defaults to CRUCIBLE_TARGET_IP"},
      {"key": "probe", "type": "string", "description": "name to attempt to resolve; defaults to a name the server cannot be authoritative for"}]'::jsonb,
    '[]'::jsonb,
$body$
local host="${CRUCIBLE_TARGET_IP:-}" probe="recursion-probe.invalid"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host) host="$2"; shift 2;;
        --probe) probe="$2"; shift 2;;
        *) shift;;
    esac
done
if [[ -z "$host" ]]; then
    LAST_ERROR="dns-server-refuses-recursion requires --host or CRUCIBLE_TARGET_IP"
    return 2
fi
if ! nc -zuw3 "$host" 53 2>/dev/null && ! nc -zw3 "$host" 53 2>/dev/null; then
    LAST_ERROR="Nothing is listening on ${host}:53"
    LAST_STUDENT_MSG="No DNS service is reachable on $host, so its recursion policy could not be checked."
    return 1
fi
# The `ra` flag in the response header is the server saying "recursion
# available", which is the property being asserted -- not whether this
# particular name happened to resolve. The probe name is .invalid, which by
# RFC 6761 can never exist, so nothing leaves the lab VLAN looking for it.
local out
out=$(dig +time=3 +tries=1 "@${host}" "$probe" A 2>&1) || true
if grep -qE '^;; flags:.*[[:space:]]ra([[:space:]]|;)' <<<"$out"; then
    LAST_ERROR="$host advertises recursion available to any client"
    LAST_STUDENT_MSG="Your DNS server offers recursive resolution to anyone, which makes it usable for amplification attacks. Restrict recursion to your own network."
    return 1
fi
if grep -qi 'connection timed out; no servers could be reached' <<<"$out"; then
    LAST_ERROR="No DNS response from $host: $out"
    LAST_STUDENT_MSG="Your DNS server did not respond on port 53."
    return 1
fi
return 0
$body$
);


DROP FUNCTION crucible_seed_action(TEXT, TEXT, TEXT, TEXT, TEXT, JSONB, JSONB, JSONB, TEXT);

COMMIT;
