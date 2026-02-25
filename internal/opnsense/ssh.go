package opnsense

import (
	"context"
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

// AssignInterface creates an OPT interface for a VLAN in OPNsense config.
// This must be done via SSH because the OPNsense REST API doesn't support
// interface creation — only VLAN and DHCP management.
//
// Steps:
//  1. Find the next available optN interface name
//  2. Edit /conf/config.xml to add the interface
//  3. Run configctl interface reconfigure
//  4. Set the IP address on the interface
func (s *SSHClient) AssignInterface(ctx context.Context, vlanTag int, ipAddr string) (string, error) {
	client, err := s.dial()
	if err != nil {
		return "", fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close()

	// Find the VLAN device name (e.g., "vmx1_vlan100")
	vlanDev := fmt.Sprintf("vmx1_vlan%d", vlanTag)

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

	// Use OPNsense's PHP-based interface assignment via configctl
	// This is safer than editing config.xml directly
	assignCmd := fmt.Sprintf(
		`configctl interface assign %s %s && `+
			`configctl interface reconfigure`,
		ifName, vlanDev,
	)

	output, err := s.runCommand(client, assignCmd)
	if err != nil {
		// Fallback: try the PHP config approach
		s.logger.Warn("configctl assign failed, trying PHP approach", "error", err, "output", output)
		return s.assignInterfacePHP(client, ifName, vlanDev, ipAddr)
	}

	// Set IP address
	ipCmd := fmt.Sprintf(
		`configctl interface address %s %s`,
		ifName, ipAddr,
	)
	if _, err := s.runCommand(client, ipCmd); err != nil {
		s.logger.Warn("configctl address failed, trying ifconfig", "error", err)
		// Fallback to ifconfig
		ifconfigCmd := fmt.Sprintf("ifconfig %s inet %s up", vlanDev, ipAddr)
		if _, err := s.runCommand(client, ifconfigCmd); err != nil {
			return ifName, fmt.Errorf("set IP on %s: %w", ifName, err)
		}
	}

	s.logger.Info("interface assigned", "interface", ifName, "ip", ipAddr)
	return ifName, nil
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
			"require_once(\"interfaces.inc\");\n"+
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

	// Write PHP script to temp file, execute, then clean up
	writeCmd := fmt.Sprintf("cat > /tmp/ss_assign.php << 'PHPEOF'\n%sPHPEOF", phpScript)
	if _, err := s.runCommand(client, writeCmd); err != nil {
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
			"require_once(\"interfaces.inc\");\n"+
			"$config = parse_config();\n"+
			"if (isset($config['interfaces']['%s'])) {\n"+
			"    $realif = $config['interfaces']['%s']['if'];\n"+
			"    unset($config['interfaces']['%s']);\n"+
			"    write_config(\"Removed interface %s for self-service pod cleanup\");\n"+
			"    interface_bring_down($realif);\n"+
			"}\n"+
			"?>\n",
		ifName, ifName, ifName, ifName,
	)

	writeCmd := fmt.Sprintf("cat > /tmp/ss_unassign.php << 'PHPEOF'\n%sPHPEOF", phpScript)
	if _, err := s.runCommand(client, writeCmd); err != nil {
		return fmt.Errorf("write PHP script: %w", err)
	}

	output, err := s.runCommand(client, "/usr/local/bin/php /tmp/ss_unassign.php")
	s.runCommandIgnoreError(client, "rm -f /tmp/ss_unassign.php")
	if err != nil {
		return fmt.Errorf("PHP interface unassign: %w (output: %s)", err, output)
	}

	s.logger.Info("interface unassigned", "interface", ifName)
	return nil
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
