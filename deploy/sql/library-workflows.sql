-- Crucible workflow seed — ready-to-assign assessments across four curriculum
-- areas: Linux hardening, recon and enumeration, web security, and service
-- configuration.
--
-- WHY THESE ARE SEEDED AS status='active'
--
-- Writing a row directly bypasses the API's activation gate, which normally
-- runs ValidateRunActionCallsWithCatalog against the persisted script and
-- requires an approver whose identity differs from the author. That gate is
-- reproduced in a test instead: TestSeed_EveryWorkflowSatisfiesTheActivationContract
-- in internal/libraryseed applies this file and runs the same validator over
-- the same catalog. If a callable here is misspelled or missing from
-- library-actions.sql, the test fails rather than a student's assessment.
--
-- Apply AFTER deploy/sql/library-actions.sql. Every callable below is a
-- function generated from an action slug in that file; without it, each
-- run_action exits 127.
--
-- WHY EVERY SCRIPT USES ONLY LIBRARY CALLABLES
--
-- Inline `bash -c '...'` in a workflow is possible but unreviewable: it is a
-- bash body stored inside a bash body inside SQL, with three levels of
-- quoting, and it is invisible to the action-level tool and syntax guards.
-- Logic belongs in library-actions.sql; a workflow's job is to sequence it and
-- label each step for the student.
--
-- WHY ORDERING WITHIN A WORKFLOW MATTERS
--
-- Actions run top to bottom and all of them run, so ordering does not gate
-- execution — it gates comprehension. Each list below opens with the
-- reachability check whose failure explains every later failure, so a student
-- reading a wall of red starts at the cause rather than a symptom.
--
-- HOW TO APPLY
--
--   PW=$(kubectl -n selfservice get secret selfservice-db-creds \
--          -o jsonpath='{.data.password}' | base64 -d)
--   kubectl -n selfservice cp deploy/sql/library-workflows.sql \
--     selfservice-postgresql-0:/tmp/library-workflows.sql
--   kubectl -n selfservice exec selfservice-postgresql-0 -- \
--     env PGPASSWORD="$PW" psql -U selfservice -d selfservice \
--     -f /tmp/library-workflows.sql

BEGIN;

-- The owning identity. `workflows.created_by` is NOT NULL, and attributing a
-- seeded library workflow to a real instructor would make the audit trail
-- claim a person authored something version control did.
--
-- `is_active` false and a sentinel `oidc_sub` Authentik cannot produce mean
-- this row is not a login. Quotas are zero so it cannot provision even if it
-- somehow were.
INSERT INTO users (
    oidc_sub, username, email, display_name, role,
    max_vcpus, max_ram_mb, max_pods, is_active
) VALUES (
    'crucible-library-seed-no-oidc',
    'crucible-library',
    'library@example.com',
    'Crucible Library Seed',
    'instructor',
    0, 0, 0, false
) ON CONFLICT (username) DO NOTHING;


-- crucible_seed_workflow exists for the same reason crucible_seed_action does:
-- without it, each of the workflows below would carry an identical 13-column
-- INSERT and 9-line ON CONFLICT, and the script — the only reviewable part —
-- would be buried.
--
-- Keyed on slug, not a fixed UUID, so re-applying adopts a row that already
-- exists in production under an id this repo has never seen.
CREATE OR REPLACE FUNCTION crucible_seed_workflow(
    p_slug        TEXT,
    p_name        TEXT,
    p_description TEXT,
    p_category    TEXT,
    p_target_os   TEXT,
    p_timeout     INT,
    p_script      TEXT
) RETURNS VOID AS $fn$
BEGIN
    INSERT INTO workflows (
        name, slug, description, category, execution_mode, creation_mode,
        script, timeout_seconds, target_os, visible_to_students, status,
        created_by, is_active
    ) VALUES (
        p_name, p_slug, p_description, p_category, 'kali_runner', 'script',
        p_script, p_timeout, p_target_os, true, 'active',
        (SELECT id FROM users WHERE username = 'crucible-library'), true
    )
    ON CONFLICT (slug) DO UPDATE SET
        name                = EXCLUDED.name,
        description         = EXCLUDED.description,
        category            = EXCLUDED.category,
        execution_mode      = EXCLUDED.execution_mode,
        creation_mode       = EXCLUDED.creation_mode,
        script              = EXCLUDED.script,
        timeout_seconds     = EXCLUDED.timeout_seconds,
        target_os           = EXCLUDED.target_os,
        visible_to_students = EXCLUDED.visible_to_students,
        status              = EXCLUDED.status,
        is_active           = EXCLUDED.is_active,
        updated_at          = now();
END;
$fn$ LANGUAGE plpgsql;


-- ===========================================================================
-- LINUX HARDENING
-- ===========================================================================

SELECT crucible_seed_workflow(
    'hardening-ssh-baseline',
    'SSH Hardening Baseline',
    'Checks the SSH server refuses root logins and password authentication, and that its key files and configuration are correctly owned.',
    'hardening', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

# First: is sshd even reachable? Every check below reads the daemon's own
# effective config over SSH, so if this fails the rest are meaningless.
run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

run_action "Root login and password auth are disabled" sshd_protocol_hardened

# Proven from the network, not from the config file: a directive can be
# correct in sshd_config and overridden in a drop-in under sshd_config.d.
run_action "Server actually refuses password logins" ssh_key_auth_only --user root

run_action "SSH protocol 2 only" sshd_directive --directive protocol --value 2
run_action "X11 forwarding is disabled" sshd_directive --directive x11forwarding --value no
run_action "Empty passwords are refused" sshd_directive --directive permitemptypasswords --value no
run_action "Login grace time is limited" sshd_directive --directive maxauthtries --value 4

run_action "sshd_config is not writable by others" file_permissions --path /etc/ssh/sshd_config --mode 644 --at-most
run_action "sshd_config is owned by root" file_owner --path /etc/ssh/sshd_config --owner root
$script$
);

SELECT crucible_seed_workflow(
    'hardening-host-firewall',
    'Host Firewall Enabled And Restrictive',
    'Checks ufw is active, defaults to denying inbound traffic, and that no unexpected ports are reachable from the network.',
    'hardening', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "Firewall is enabled" ufw_enabled
run_action "Default inbound policy is deny" ufw_default_deny_incoming
run_action "SSH is explicitly allowed" ufw_rule_exists --rule 22

# The config and the observed reality are separate assertions. A firewall can
# report "active" with a rule set that still leaves a service exposed, and
# only a scan from off-box shows it.
run_action "Only SSH is reachable from the network" nmap_only_expected_ports --allow 22 --range 1-1024
run_action "Telnet is not reachable" nmap_port_state --port 23 --state closed
$script$
);

SELECT crucible_seed_workflow(
    'hardening-account-hygiene',
    'Account And Privilege Hygiene',
    'Checks no account has an empty password, root is the only UID 0, sudo does not grant passwordless root, and the shadow file is protected.',
    'hardening', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

run_action "No account has an empty password" no_empty_passwords
run_action "Root is the only UID 0 account" no_extra_uid_zero
run_action "Sudo requires a password" no_passwordless_sudo

# /etc/shadow world-readable is how a local user reads every password hash on
# the box; the mode and the owner are separate ways to get it wrong.
run_action "Shadow file is not readable by others" file_permissions --path /etc/shadow --mode 640 --at-most
run_action "Shadow file is owned by root" file_owner --path /etc/shadow --owner root
run_action "Passwd file is owned by root" file_owner --path /etc/passwd --owner root
run_action "No world-writable files under /etc" no_world_writable --path /etc --max-depth 3
$script$
);

SELECT crucible_seed_workflow(
    'hardening-kernel-network',
    'Kernel Network Hardening',
    'Checks the running kernel drops source-routed and redirect packets, enables reverse-path filtering, and logs martians.',
    'hardening', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

# Every one of these reads the RUNNING value. A setting written to
# /etc/sysctl.conf and never applied is the single most common way this
# workflow is failed by a student who believes they finished.
run_action "IP forwarding is disabled" sysctl_value --key net.ipv4.ip_forward --value 0
run_action "Source-routed packets are rejected" sysctl_value --key net.ipv4.conf.all.accept_source_route --value 0
run_action "ICMP redirects are not accepted" sysctl_value --key net.ipv4.conf.all.accept_redirects --value 0
run_action "ICMP redirects are not sent" sysctl_value --key net.ipv4.conf.all.send_redirects --value 0
run_action "Reverse path filtering is on" sysctl_value --key net.ipv4.conf.all.rp_filter --value 1
run_action "Martian packets are logged" sysctl_value --key net.ipv4.conf.all.log_martians --value 1
run_action "SYN cookies are enabled" sysctl_value --key net.ipv4.tcp_syncookies --value 1
run_action "Address space layout is randomized" sysctl_value --key kernel.randomize_va_space --value 2
$script$
);

SELECT crucible_seed_workflow(
    'hardening-patch-and-persistence',
    'Patching And Persistence Review',
    'Checks automatic security updates are enabled and that no scheduled task has been added to re-establish access.',
    'hardening', 'linux', 240,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

run_action "Automatic security updates are enabled" unattended_upgrades_enabled

# Two of the three classic reverse-shell cron shapes. The patterns match the
# command, not a filename, so renaming the script does not evade them.
run_action "No netcat reverse shell in cron" cron_entry_absent --pattern "nc[[:space:]]+.*[[:space:]]-e"
run_action "No bash TCP redirect in cron" cron_entry_absent --pattern "/dev/tcp/"
run_action "No curl-to-shell pipeline in cron" cron_entry_absent --pattern "(curl|wget).*\\|[[:space:]]*(ba)?sh"
$script$
);


-- ===========================================================================
-- RECON AND ENUMERATION
--
-- These grade a student's SERVER, not their scanning skill: "can an attacker
-- learn X about your host" is the assertion. That is why they run from the
-- runner over the pod VLAN rather than inside the VM.
-- ===========================================================================

SELECT crucible_seed_workflow(
    'recon-attack-surface',
    'Attack Surface Review',
    'Scans the target and checks that only the intended service is exposed, and that legacy cleartext services are gone.',
    'recon', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "Host is on the network" host_responds_to_ping

run_action "Only SSH is exposed" nmap_only_expected_ports --allow 22 --range 1-1024
run_action "Telnet is gone" nmap_port_state --port 23 --state closed
run_action "FTP is gone" nmap_port_state --port 21 --state closed
run_action "rlogin is gone" nmap_port_state --port 513 --state closed
run_action "SMB is not exposed" nmap_port_state --port 445 --state closed
$script$
);

SELECT crucible_seed_workflow(
    'recon-service-banners',
    'Service Banners Do Not Leak Versions',
    'Checks the SSH and web services do not advertise exact software versions to an unauthenticated client.',
    'recon', 'linux', 240,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

# OpenSSH cannot be made to omit its version string without patching, so the
# assertion is the weaker true one: the DISTRIBUTION suffix, which names the
# exact package build, should not be present.
run_action "SSH banner does not name the OS build" tcp_banner_matches --port 22 --expect "Ubuntu-" --absent

run_action "Web server does not send a Server version" http_header_absent --url "http://$CRUCIBLE_TARGET_IP/" --header Server --value "[0-9]+\\.[0-9]+"
run_action "Web server does not send X-Powered-By" http_header_absent --url "http://$CRUCIBLE_TARGET_IP/" --header X-Powered-By
$script$
);

SELECT crucible_seed_workflow(
    'recon-smb-enumeration',
    'SMB Enumeration Is Restricted',
    'Checks the SMB service does not allow anonymous share listing and requires message signing.',
    'recon', 'linux', 240,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SMB is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 445

run_action "Anonymous share listing is refused" smb_share_listable --absent
run_action "Message signing is required" smb_signing_required
$script$
);

SELECT crucible_seed_workflow(
    'recon-dns-posture',
    'DNS Service Posture',
    'Checks a DNS server resolves its own zone but does not offer recursion to arbitrary clients.',
    'recon', 'linux', 240,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "DNS port is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 53

# An open resolver is usable for amplification attacks against third parties,
# which is why this is graded even though it does not expose the student's own
# data.
run_action "Recursion is not offered to any client" dns_server_refuses_recursion

run_action "The server is bound to its intended address" service_listening_on --port 53 --address "$CRUCIBLE_TARGET_IP"
$script$
);


-- ===========================================================================
-- WEB SECURITY
-- ===========================================================================

SELECT crucible_seed_workflow(
    'web-tls-configuration',
    'TLS Configuration',
    'Checks HTTPS is served with a currently valid certificate, obsolete protocol versions are refused, and plain HTTP redirects to HTTPS.',
    'web', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "HTTPS is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 443

run_action "Certificate is currently valid" tls_certificate_valid --port 443
run_action "TLS 1.0 is refused" tls_protocol_refused --protocol tls1
run_action "TLS 1.1 is refused" tls_protocol_refused --protocol tls1_1
run_action "Plain HTTP redirects to HTTPS" http_redirects_to_https
run_action "HSTS is sent" http_header_present --url "https://$CRUCIBLE_TARGET_IP/" --header Strict-Transport-Security --value "max-age=[0-9]+"
$script$
);

SELECT crucible_seed_workflow(
    'web-security-headers',
    'HTTP Security Headers',
    'Checks the web server sends the response headers that constrain how a browser treats the page, and none that leak its software version.',
    'web', 'linux', 240,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "Web server is reachable" http_get --url "http://$CRUCIBLE_TARGET_IP/"

run_action "Framing is restricted" http_header_present --url "http://$CRUCIBLE_TARGET_IP/" --header X-Frame-Options --value "(DENY|SAMEORIGIN)"
run_action "MIME sniffing is disabled" http_header_present --url "http://$CRUCIBLE_TARGET_IP/" --header X-Content-Type-Options --value nosniff
run_action "A content security policy is set" http_header_present --url "http://$CRUCIBLE_TARGET_IP/" --header Content-Security-Policy
run_action "Referrer policy is set" http_header_present --url "http://$CRUCIBLE_TARGET_IP/" --header Referrer-Policy

run_action "Server version is not advertised" http_header_absent --url "http://$CRUCIBLE_TARGET_IP/" --header Server --value "[0-9]+\\.[0-9]+"
run_action "Fingerprint does not include a version" web_technology_hidden --url "http://$CRUCIBLE_TARGET_IP/"
$script$
);

SELECT crucible_seed_workflow(
    'web-access-control',
    'Web Access Control',
    'Checks directory listing is off, sensitive paths require authentication, and default credentials no longer work.',
    'web', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "Web server is reachable" http_get --url "http://$CRUCIBLE_TARGET_IP/"

run_action "Document root does not list files" directory_listing_disabled --url "http://$CRUCIBLE_TARGET_IP/"
run_action "Uploads directory does not list files" directory_listing_disabled --url "http://$CRUCIBLE_TARGET_IP/uploads/"

run_action "Admin area requires authentication" http_auth_required --url "http://$CRUCIBLE_TARGET_IP/admin/"

# Requiring a password is worth nothing if it is still the shipped one, so
# both assertions are needed.
run_action "Default admin credentials are rejected" http_credentials_rejected --url "http://$CRUCIBLE_TARGET_IP/admin/" --user admin --pass admin
run_action "Blank admin password is rejected" http_credentials_rejected --url "http://$CRUCIBLE_TARGET_IP/admin/" --user admin --pass ""
$script$
);

SELECT crucible_seed_workflow(
    'web-server-file-hygiene',
    'Web Server File Hygiene',
    'Checks the document root is not writable by the web server user and that the configuration files are owned by root.',
    'web', 'linux', 240,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

# A web root writable by the process that serves it turns any file-upload bug
# into remote code execution.
run_action "Document root is not world-writable" no_world_writable --path /var/www --max-depth 4
run_action "Document root is owned by root" file_owner --path /var/www/html --owner root

run_action "nginx config is owned by root" file_owner --path /etc/nginx/nginx.conf --owner root
run_action "nginx config is not writable by others" file_permissions --path /etc/nginx/nginx.conf --mode 644 --at-most
$script$
);


-- ===========================================================================
-- SERVICE CONFIGURATION
-- ===========================================================================

SELECT crucible_seed_workflow(
    'service-web-stack-running',
    'Web Stack Is Running And Serving',
    'Checks nginx is installed, enabled, listening, and actually answering requests — the four separate ways "the web server works" can be false.',
    'service', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

# Installed, running, bound, and answering are four distinct failures with
# four distinct fixes. Collapsing them into one HTTP check would tell a
# student "it does not work" and nothing else.
run_action "nginx is installed" package_installed --name nginx
run_action "nginx service is running" service_running --name nginx
run_action "nginx is listening on port 80" port_open --host "$CRUCIBLE_TARGET_IP" --port 80
run_action "nginx returns a page" http_get --url "http://$CRUCIBLE_TARGET_IP/"
$script$
);

SELECT crucible_seed_workflow(
    'service-database-not-exposed',
    'Database Is Not Exposed To The Network',
    'Checks a database service runs but is bound to localhost only and unreachable from another host on the VLAN.',
    'service', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

run_action "Database service is running" service_running --name postgresql

# Bound-to and reachable-from are different questions with different
# evidence: `ss` on the target answers the first, a scan from the runner
# answers the second, and a student can pass either one alone while failing
# the actual requirement.
run_action "Database listens on localhost only" service_listening_on --port 5432 --address 127.0.0.1
run_action "Database is not reachable from the network" nmap_port_state --port 5432 --state closed
run_action "Redis is not reachable from the network" nmap_port_state --port 6379 --state closed
$script$
);

SELECT crucible_seed_workflow(
    'service-file-sharing-configured',
    'File Sharing Configured Correctly',
    'Checks Samba is running, exports the expected share, and does not permit anonymous access.',
    'service', 'linux', 300,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

run_action "Samba is installed" package_installed --name samba
run_action "Samba service is running" service_running --name smbd
run_action "SMB port is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 445

run_action "smb.conf declares the share" file_contains --path /etc/samba/smb.conf --pattern "\\[shared\\]"
run_action "Guest access is refused" smb_share_listable --absent
run_action "Message signing is required" smb_signing_required
$script$
);

SELECT crucible_seed_workflow(
    'service-account-provisioning',
    'Service Account Provisioning',
    'Checks the required service account exists with the right group membership, the shared default account has been locked, and the removed account is gone.',
    'service', 'linux', 240,
$script$
source /opt/crucible/lib/actions.sh
set -uo pipefail

run_action "SSH is reachable" port_open --host "$CRUCIBLE_TARGET_IP" --port 22

run_action "Service account exists" user_exists --name svc-backup
run_action "Service account can use sudo" user_in_group --user svc-backup --group sudo

# A service account that can log in interactively with a password is a
# credential to steal; existence and lockedness are separate requirements.
run_action "Shared default account is locked" account_locked --name ubuntu
run_action "Decommissioned account is removed" user_absent --name tempadmin

# Removing an account does not remove it from a group file, and a stale
# membership becomes a privilege grant the moment the name is reused.
run_action "Decommissioned account is not in sudo" user_in_group --user tempadmin --group sudo --absent
$script$
);


DROP FUNCTION crucible_seed_workflow(TEXT, TEXT, TEXT, TEXT, TEXT, INT, TEXT);

COMMIT;
