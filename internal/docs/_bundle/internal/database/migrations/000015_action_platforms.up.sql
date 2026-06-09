-- Add supported_platforms to actions for OS-aware action filtering
ALTER TABLE actions ADD COLUMN supported_platforms JSONB NOT NULL DEFAULT '["any"]';

-- Update existing seeded library actions with correct platform tags
UPDATE actions SET supported_platforms = '["any"]' WHERE is_library = true AND action_category = 'network';
UPDATE actions SET supported_platforms = '["any"]' WHERE is_library = true AND slug IN ('http-get', 'http-post', 'port-open', 'port-closed', 'dns-resolves', 'nmap-service', 'smb-share-accessible');
UPDATE actions SET supported_platforms = '["linux"]' WHERE is_library = true AND slug IN ('ssh-exec', 'ssh-denied', 'command-check', 'wait-for', 'git-clone', 'git-push');
UPDATE actions SET supported_platforms = '["linux:ubuntu", "linux:debian"]' WHERE is_library = true AND slug IN ('file-contains', 'service-running', 'package-installed', 'ufw-enabled', 'ufw-rule-exists');

CREATE INDEX idx_actions_platforms ON actions USING GIN (supported_platforms);
