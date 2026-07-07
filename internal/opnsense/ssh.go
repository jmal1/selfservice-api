package opnsense

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHClient handles OPNsense operations that require SSH (interface assignment).
type SSHClient struct {
	config Config
	logger *slog.Logger
}

// NewSSHClient creates an SSH client for OPNsense.
func NewSSHClient(cfg Config, logger *slog.Logger) *SSHClient {
	return &SSHClient{config: cfg, logger: logger}
}

// dial establishes an SSH connection to OPNsense.
func (s *SSHClient) dial() (*ssh.Client, error) {
	var authMethods []ssh.AuthMethod

	if len(s.config.SSHKey) > 0 {
		signer, err := ssh.ParsePrivateKey(s.config.SSHKey)
		if err != nil {
			return nil, fmt.Errorf("parse SSH key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if s.config.SSHPassword != "" {
		authMethods = append(authMethods, ssh.Password(s.config.SSHPassword))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no SSH auth method configured")
	}

	sshConfig := &ssh.ClientConfig{
		User:            s.config.SSHUser,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // OPNsense host key may change
		Timeout:         10 * time.Second,
	}

	host := s.config.SSHHost
	if !strings.Contains(host, ":") {
		host += ":22"
	}

	return ssh.Dial("tcp", host, sshConfig)
}

// runCommand executes a command on OPNsense via SSH.
func (s *SSHClient) runCommand(client *ssh.Client, cmd string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("create SSH session: %w", err)
	}
	defer session.Close()

	output, err := session.CombinedOutput(cmd)
	if err != nil {
		return string(output), fmt.Errorf("SSH command failed: %w (output: %s)", err, string(output))
	}
	return string(output), nil
}

// runCommandIgnoreError executes a command and ignores any errors (for cleanup).
func (s *SSHClient) runCommandIgnoreError(client *ssh.Client, cmd string) {
	session, err := client.NewSession()
	if err != nil {
		return
	}
	defer session.Close()
	_, _ = session.CombinedOutput(cmd)
}

// writePHPScript writes a PHP script to a temp file on OPNsense using base64
// encoding to avoid heredoc/csh shell quoting issues.
func (s *SSHClient) writePHPScript(client *ssh.Client, path, script string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	cmd := fmt.Sprintf("echo %s | /usr/bin/base64 -d > %s", encoded, path)
	_, err := s.runCommand(client, cmd)
	return err
}

// AssignInterface creates an OPT interface for a VLAN in OPNsense config.
// This must be done via SSH because the OPNsense REST API doesn't support
// interface creation — only VLAN and DHCP management.
// Uses PHP to edit config.xml directly (configctl interface assign creates
// empty entries and doesn't work correctly).
func (s *SSHClient) AssignInterface(ctx context.Context, vlanTag int, ipAddr string) (string, error) {
	client, err := s.dial()
	if err != nil {
		return "", fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close()

	// Look up the actual VLAN device name from OPNsense config
	vlanDev, err := s.findVLANDevice(client, vlanTag)
	if err != nil {
		return "", fmt.Errorf("find VLAN device for tag %d: %w", vlanTag, err)
	}

	// Find next available opt interface
	ifName, err := s.findNextInterface(client)
	if err != nil {
		return "", fmt.Errorf("find next interface: %w", err)
	}

	s.logger.Info("assigning OPNsense interface",
		"vlan_tag", vlanTag,
		"vlan_dev", vlanDev,
		"interface", ifName,
		"ip", ipAddr,
	)

	return s.assignInterfacePHP(client, ifName, vlanDev, ipAddr)
}

// FindInterfaceByVLAN returns the OPNsense interface key (e.g., opt4) bound
// to a VLAN tag, or empty string when no interface is assigned.
func (s *SSHClient) FindInterfaceByVLAN(ctx context.Context, vlanTag int) (string, error) {
	client, err := s.dial()
	if err != nil {
		return "", fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close()

	phpScript := fmt.Sprintf(
		"<?php\n"+
			"require_once(\"config.inc\");\n"+
			"$config = parse_config();\n"+
			"$vlan_tag = %d;\n"+
			"$vlan_dev = '';\n"+
			"if (isset($config['vlans']['vlan'])) {\n"+
			"    foreach ($config['vlans']['vlan'] as $v) {\n"+
			"        if (isset($v['tag']) && intval($v['tag']) === $vlan_tag) {\n"+
			"            $vlan_dev = $v['vlanif'];\n"+
			"            break;\n"+
			"        }\n"+
			"    }\n"+
			"}\n"+
			"if ($vlan_dev === '') { exit(0); }\n"+
			"foreach ($config['interfaces'] as $ifname => $iface) {\n"+
			"    if (isset($iface['if']) && $iface['if'] === $vlan_dev) {\n"+
			"        echo $ifname;\n"+
			"        exit(0);\n"+
			"    }\n"+
			"}\n"+
			"?>\n",
		vlanTag,
	)
	if err := s.writePHPScript(client, "/tmp/ss_find_if_vlan.php", phpScript); err != nil {
		return "", fmt.Errorf("write PHP script: %w", err)
	}

	output, err := s.runCommand(client, "/usr/local/bin/php /tmp/ss_find_if_vlan.php")
	s.runCommandIgnoreError(client, "rm -f /tmp/ss_find_if_vlan.php")
	if err != nil {
		return "", fmt.Errorf("PHP find interface by VLAN: %w (output: %s)", err, output)
	}
	return strings.TrimSpace(output), nil
}

// assignInterfacePHP uses PHP to edit the OPNsense config directly.
func (s *SSHClient) assignInterfacePHP(client *ssh.Client, ifName, vlanDev, ipAddr string) (string, error) {
	// Use OPNsense's built-in PHP config utility
	// Write to a temp file to avoid shell quoting issues on FreeBSD csh
	ipOnly := strings.Split(ipAddr, "/")[0]
	upperIfName := strings.ToUpper(ifName)

	phpScript := fmt.Sprintf(
		"<?php\n"+
			"require_once(\"config.inc\");\n"+
			"require_once(\"util.inc\");\n"+
			"require_once(\"interfaces.inc\");\n"+
			"require_once(\"system.inc\");\n"+
			"$config = parse_config();\n"+
			"$config['interfaces']['%s'] = array(\n"+
			"    'if' => '%s',\n"+
			"    'descr' => '%s',\n"+
			"    'enable' => '1',\n"+
			"    'ipaddr' => '%s',\n"+
			"    'subnet' => '24',\n"+
			"    'spoofmac' => '',\n"+
			");\n"+
			"write_config(\"Added interface %s for self-service pod\");\n"+
			"interface_configure(false, '%s');\n"+
			"?>\n",
		ifName, vlanDev, upperIfName, ipOnly,
		ifName, ifName,
	)

	// Write PHP script to temp file using base64, execute, then clean up
	if err := s.writePHPScript(client, "/tmp/ss_assign.php", phpScript); err != nil {
		return ifName, fmt.Errorf("write PHP script: %w", err)
	}

	output, err := s.runCommand(client, "/usr/local/bin/php /tmp/ss_assign.php")
	s.runCommandIgnoreError(client, "rm -f /tmp/ss_assign.php")
	if err != nil {
		return ifName, fmt.Errorf("PHP interface assign: %w (output: %s)", err, output)
	}

	return ifName, nil
}

// UnassignInterface removes an OPT interface from OPNsense config.
func (s *SSHClient) UnassignInterface(ctx context.Context, ifName string) error {
	client, err := s.dial()
	if err != nil {
		return fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close()

	s.logger.Info("unassigning OPNsense interface", "interface", ifName)

	phpScript := fmt.Sprintf(
		"<?php\n"+
			"require_once(\"config.inc\");\n"+
			"require_once(\"util.inc\");\n"+
			"require_once(\"interfaces.inc\");\n"+
			"$config = parse_config();\n"+
			"if (isset($config['interfaces']['%s'])) {\n"+
			"    unset($config['interfaces']['%s']);\n"+
			"    write_config(\"Removed interface %s for self-service pod cleanup\");\n"+
			"}\n"+
			"?>\n",
		ifName, ifName, ifName,
	)

	if err := s.writePHPScript(client, "/tmp/ss_unassign.php", phpScript); err != nil {
		return fmt.Errorf("write PHP script: %w", err)
	}

	output, err := s.runCommand(client, "/usr/local/bin/php /tmp/ss_unassign.php")
	s.runCommandIgnoreError(client, "rm -f /tmp/ss_unassign.php")
	if err != nil {
		return fmt.Errorf("PHP interface unassign: %w (output: %s)", err, output)
	}

	// Apply config change so OPNsense releases the interface
	s.runCommandIgnoreError(client, "configctl interface reconfigure")
	s.logger.Info("interface unassigned", "interface", ifName)
	return nil
}

// findVLANDevice looks up the actual kernel device name for a VLAN tag
// from OPNsense's config.xml (e.g., tag 100 → "vlan01").
func (s *SSHClient) findVLANDevice(client *ssh.Client, vlanTag int) (string, error) {
	phpScript := fmt.Sprintf(
		"<?php\n"+
			"require_once(\"config.inc\");\n"+
			"$config = parse_config();\n"+
			"if (isset($config['vlans']['vlan'])) {\n"+
			"    foreach ($config['vlans']['vlan'] as $v) {\n"+
			"        if (isset($v['tag']) && intval($v['tag']) === %d) {\n"+
			"            echo $v['vlanif'];\n"+
			"            exit(0);\n"+
			"        }\n"+
			"    }\n"+
			"}\n"+
			"echo 'NOT_FOUND';\n"+
			"?>\n",
		vlanTag,
	)

	if err := s.writePHPScript(client, "/tmp/ss_find_vlan.php", phpScript); err != nil {
		return "", fmt.Errorf("write PHP script: %w", err)
	}

	output, err := s.runCommand(client, "/usr/local/bin/php /tmp/ss_find_vlan.php")
	s.runCommandIgnoreError(client, "rm -f /tmp/ss_find_vlan.php")
	if err != nil {
		return "", fmt.Errorf("PHP find VLAN device: %w (output: %s)", err, output)
	}

	dev := strings.TrimSpace(output)
	if dev == "" || dev == "NOT_FOUND" {
		return "", fmt.Errorf("no VLAN device found for tag %d", vlanTag)
	}
	return dev, nil
}

// findNextInterface determines the next available optN name.
func (s *SSHClient) findNextInterface(client *ssh.Client) (string, error) {
	// List existing interfaces from config
	output, err := s.runCommand(client,
		`grep -oP '<(opt\d+)>' /conf/config.xml | sort -u | tail -1`)
	if err != nil || strings.TrimSpace(output) == "" {
		return "opt1", nil // first optional interface
	}

	// Extract the highest opt number
	trimmed := strings.TrimSpace(output)
	trimmed = strings.TrimPrefix(trimmed, "<")
	trimmed = strings.TrimSuffix(trimmed, ">")
	// e.g., "opt5" -> "5"
	numStr := strings.TrimPrefix(trimmed, "opt")
	var num int
	fmt.Sscanf(numStr, "%d", &num)

	return fmt.Sprintf("opt%d", num+1), nil
}

// UnassignInterfaceByVLAN finds and removes the interface assigned to a VLAN tag.
// Returns the OPNsense interface name (e.g., "opt1") so callers can clean up Kea.
func (s *SSHClient) UnassignInterfaceByVLAN(ctx context.Context, vlanTag int) (string, error) {
	client, err := s.dial()
	if err != nil {
		return "", fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close()

	s.logger.Info("finding OPNsense interface by VLAN tag", "vlan_tag", vlanTag)

	// PHP script that looks up the VLAN device from config.xml's VLAN table,
	// then finds and removes the interface using that device name.
	phpScript := fmt.Sprintf(
		"<?php\n"+
			"require_once(\"config.inc\");\n"+
			"require_once(\"util.inc\");\n"+
			"require_once(\"interfaces.inc\");\n"+
			"$config = parse_config();\n"+
			"$vlan_tag = %d;\n"+
			"$vlan_dev = '';\n"+
			"if (isset($config['vlans']['vlan'])) {\n"+
			"    foreach ($config['vlans']['vlan'] as $v) {\n"+
			"        if (isset($v['tag']) && intval($v['tag']) === $vlan_tag) {\n"+
			"            $vlan_dev = $v['vlanif'];\n"+
			"            break;\n"+
			"        }\n"+
			"    }\n"+
			"}\n"+
			"if ($vlan_dev === '') { echo \"no_vlan\"; exit(0); }\n"+
			"$found = false;\n"+
			"foreach ($config['interfaces'] as $ifname => $iface) {\n"+
			"    if (isset($iface['if']) && $iface['if'] === $vlan_dev) {\n"+
			"        unset($config['interfaces'][$ifname]);\n"+
			"        write_config(\"Removed interface $ifname (VLAN $vlan_tag) for self-service pod cleanup\");\n"+
			"        echo \"unassigned:$ifname\";\n"+
			"        $found = true;\n"+
			"        break;\n"+
			"    }\n"+
			"}\n"+
			"if (!$found) { echo \"not_found\"; }\n"+
			"?>\n",
		vlanTag,
	)

	if err := s.writePHPScript(client, "/tmp/ss_unassign_vlan.php", phpScript); err != nil {
		return "", fmt.Errorf("write PHP script: %w", err)
	}

	output, err := s.runCommand(client, "/usr/local/bin/php /tmp/ss_unassign_vlan.php")
	s.runCommandIgnoreError(client, "rm -f /tmp/ss_unassign_vlan.php")
	if err != nil {
		return "", fmt.Errorf("PHP interface unassign by VLAN: %w (output: %s)", err, output)
	}

	trimmed := strings.TrimSpace(output)
	if strings.HasPrefix(trimmed, "unassigned:") {
		ifName := strings.TrimPrefix(trimmed, "unassigned:")
		// Apply config change so OPNsense releases the interface
		s.runCommandIgnoreError(client, "configctl interface reconfigure")
		s.logger.Info("interface unassigned by VLAN", "interface", ifName, "vlan_tag", vlanTag)
		return ifName, nil
	} else if trimmed == "no_vlan" {
		s.logger.Info("no VLAN found in config for tag", "vlan_tag", vlanTag)
	} else {
		s.logger.Info("no interface found for VLAN tag", "vlan_tag", vlanTag)
	}
	return "", nil
}

// CheckConnectivity verifies SSH access to OPNsense.
func (s *SSHClient) CheckConnectivity(ctx context.Context) error {
	client, err := s.dial()
	if err != nil {
		return err
	}
	defer client.Close()

	output, err := s.runCommand(client, "echo ok")
	if err != nil {
		return err
	}
	if !strings.Contains(output, "ok") {
		return fmt.Errorf("unexpected SSH response: %s", output)
	}
	return nil
}
