package unattend

// cloudinit.go generates the CIDATA (NoCloud) seed ISO that drives an Ubuntu
// Server 24.04 subiquity autoinstall.
//
// The seed ISO carries two files at its root, "user-data" and "meta-data",
// and MUST have the volume label "CIDATA" or cloud-init's NoCloud datasource
// will not discover it. The user-data begins with the literal "#cloud-config"
// line followed by an autoinstall: block.

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// cidataVolumeLabel is the exact volume label cloud-init's NoCloud datasource
// looks for. Do not change this.
const cidataVolumeLabel = "CIDATA"

// crucibleSSHKeygenUnit recreates OpenSSH host keys when they are absent.
//
// generalizeScript() deletes /etc/ssh/ssh_host_* so that each clone gets its own
// host identity (acceptance test C4-5: two clones of one template must present
// DIFFERENT host key fingerprints). Ubuntu 24.04 ships nothing that puts them
// back: there is no ssh-keygen.service, ssh.service only runs "sshd -t", and ssh
// is socket-activated via ssh.socket. Verified on the first real ISO build -
// "systemctl is-enabled ssh-keygen.service" returned not-found.
//
// cloud-init's cc_ssh module does generate missing host keys on a new instance,
// but it races socket activation, so a clone can come up with sshd failing its
// config test and refusing connections until cloud-init catches up. This unit
// removes the race and does not depend on cloud-init running at all.
//
// The ConditionPathExists guard makes it a no-op on every subsequent boot, so it
// can never rotate a working host key out from under a running pod.
//
// It orders itself before ssh.SERVICE and deliberately NOT before ssh.SOCKET.
// Ordering before ssh.socket looks stricter but is a systemd ordering cycle,
// and it took SSH down completely on the first real ISO build:
//
//	sockets.target: Found ordering cycle on ssh.socket/start
//	sockets.target: Job ssh.socket/start deleted to break ordering cycle
//
// The cycle is: this unit is WantedBy=multi-user.target, so it inherits the
// default After=basic.target; basic.target is After sockets.target; and
// ssh.socket is Before=sockets.target. Asking to run before ssh.socket
// therefore closes the loop, and systemd breaks it by DELETING ssh.socket's
// start job - so nothing ever listens on port 22. Note the condition above was
// false on that build (host keys existed), which did not help: systemd resolves
// ordering cycles before it evaluates conditions, so a unit that gets skipped
// anyway can still take SSH out.
//
// Ordering before ssh.service is both cycle-free and sufficient. Under socket
// activation ssh.socket only binds the port; ssh.service is what execs sshd and
// reads the host keys. A connection arriving before this unit has run triggers
// ssh.service, which systemd then orders after us, so sshd still never starts
// without keys.
const crucibleSSHKeygenUnit = `[Unit]
Description=Regenerate missing OpenSSH host keys (Crucible)
ConditionPathExists=!/etc/ssh/ssh_host_ed25519_key
Before=ssh.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/ssh-keygen -A

[Install]
WantedBy=multi-user.target
`

// sshKeygenUnitName is the systemd unit installed by crucibleSSHKeygenUnit.
const sshKeygenUnitName = "crucible-regen-ssh-hostkeys.service"

// sudoersPath is where the passwordless-sudo drop-in lands for a given user.
// The 90- prefix puts it after Ubuntu's own drop-ins so it wins.
func sudoersPath(username string) string {
	return "/etc/sudoers.d/90-crucible-" + username
}

// safeUsernameRe is the subset of usernames this package will emit into a
// sudoers file and a shell command. Deliberately stricter than useradd: these
// values reach both YAML and "sh -c", so anything outside this set is refused
// at generation time rather than escaped and hoped for.
var safeUsernameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// inTarget wraps a shell script so curtin runs it inside the installed system
// rather than the installer environment.
func inTarget(script string) string {
	return "curtin in-target --target=/target -- sh -c " + shellQuote(script)
}

// writeFileScript emits a shell fragment that writes content to path with mode.
func writeFileScript(path, content, mode string) string {
	dir := "."
	if i := strings.LastIndex(path, "/"); i > 0 {
		dir = path[:i]
	}
	return "mkdir -p " + shellQuote(dir) +
		" && printf '%s' " + shellQuote(content) + " > " + shellQuote(path) +
		" && chmod " + mode + " " + shellQuote(path)
}

type ciIdentity struct {
	Hostname string `yaml:"hostname"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type ciSSH struct {
	InstallServer bool `yaml:"install-server"`
}

type ciKeyboard struct {
	Layout string `yaml:"layout"`
}

// ciApt mirrors curtin's apt config, which subiquity passes straight through.
//
// Proxy is a SCALAR URL. It is emphatically not a mapping: curtin's schema puts
// proxy / http_proxy / https_proxy side by side as sibling strings. Nesting
// http_proxy under proxy does not fail - curtin stringifies whatever it is
// given, so the installed system ends up with the literal Python dict repr
//
//	Acquire::http::Proxy "{'http_proxy': 'http://10.10.30.20:3142', ...}";
//
// in /etc/apt/apt.conf.d/90curtin-aptproxy, which breaks every apt command on
// the template and on every student clone made from it. Observed in production
// on the first real build. TestBuildSeedISO_CIData_AptProxyIsScalar guards it.
//
// Only the http proxy is set. The apt cache is apt-cacher-ng, and pointing
// https_proxy at it breaks https repositories rather than caching them.
type ciApt struct {
	Proxy string `yaml:"proxy,omitempty"`
}

type ciAutoinstall struct {
	Version       int        `yaml:"version"`
	Identity      ciIdentity `yaml:"identity"`
	SSH           ciSSH      `yaml:"ssh"`
	Packages      []string   `yaml:"packages"`
	Locale        string     `yaml:"locale"`
	Keyboard      ciKeyboard `yaml:"keyboard"`
	Timezone      string     `yaml:"timezone"`
	Apt           *ciApt     `yaml:"apt,omitempty"`
	EarlyCommands []string   `yaml:"early-commands"`
	LateCommands  []string   `yaml:"late-commands"`
	Shutdown      string     `yaml:"shutdown"`
}

type ciDocument struct {
	Autoinstall ciAutoinstall `yaml:"autoinstall"`
}

// crucibleCloudCfg is written into the installed system so that on first boot
// cloud-init (a) reads per-clone metadata from the VMware guestinfo datasource
// and (b) applies passwords to the "student" default user. This is the Linux
// half of the "default user must be student" contract.
const crucibleCloudCfg = `datasource_list: [ VMware, NoCloud, None ]
system_info:
  default_user:
    name: student
    lock_passwd: false
    gecos: Crucible Student
    sudo: ["ALL=(ALL) NOPASSWD:ALL"]
    shell: /bin/bash
`

// autoConfirmScript answers subiquity's destructive-operation safeguard.
//
// Delivering an autoinstall config over cloud-init is NOT enough to get a
// hands-off install. subiquity blocks before it touches the disk and prints:
//
//	Confirmation is required to continue.
//	Add 'autoinstall' to your kernel command line to avoid this
//	Continue with autoinstall? (yes|no)
//
// and the only bypass is the kernel command line
// (subiquity/server/controllers/install.py):
//
//	if not self.app.interactive:
//	    if "autoinstall" in self.app.kernel_cmdline:
//	        await self.model.confirm()
//	self.app.update_state(ApplicationState.NEEDS_CONFIRMATION)
//	if await self.model.wait_confirmation():
//	    break
//
// We cannot set that argument: it lives in the installer ISO's GRUB config, and
// remastering the ISO needs an El Torito boot-catalog writer we do not have
// (see RemasterPreseedISO / ErrRemasterUnsupported). Observed consequence
// before this existed: the VM booted, sat at the prompt indefinitely, and every
// external signal said the install was progressing.
//
// So we answer the prompt from inside the ephemeral installer instead, using
// subiquity's own client/server API (the same POST /meta/confirm the console
// client issues when a human types "yes").
//
// Three deliberate properties:
//
//   - It waits for state NEEDS_CONFIRMATION before confirming. Confirming early
//     would work — model.confirm() sets an asyncio.Event that wait_confirmation()
//     later observes as already set — but it also broadcasts INSTALL_CONFIRMED,
//     and firing that before the installer expects it is a needless risk.
//   - It is written in python3, not curl. python3 is guaranteed present because
//     subiquity itself is a python application; curl is not.
//   - It can never fail the install. early-commands abort the run on a non-zero
//     exit, so this backgrounds itself, swallows every error, and gives up after
//     ~20 minutes. A failure here degrades to the old behaviour (the install
//     waits at the prompt and the provisioner reports a timeout) rather than
//     turning a recoverable stall into an immediate abort.
const autoConfirmScript = `python3 -c '
import socket, time
SOCKETS = ["/run/subiquity/socket", "/run/subiquity/server.sock"]
def call(path, verb):
    for p in SOCKETS:
        try:
            c = socket.socket(socket.AF_UNIX)
            c.settimeout(10)
            c.connect(p)
            c.sendall(("%s %s HTTP/1.1\r\nHost: l\r\nContent-Length: 0\r\nConnection: close\r\n\r\n" % (verb, path)).encode())
            buf = b""
            while True:
                chunk = c.recv(65536)
                if not chunk:
                    break
                buf += chunk
            c.close()
            return buf
        except Exception:
            pass
    return b""
for _ in range(240):
    if b"NEEDS_CONFIRMATION" in call("/meta/status", "GET"):
        call("/meta/confirm?tty=%22%2Fdev%2Ftty1%22", "POST")
        break
    time.sleep(5)
' >/dev/null 2>&1 &`

// buildCloudInitUserData renders the #cloud-config user-data document for an
// Ubuntu autoinstall. The password is stored as a SHA-512 crypt hash, never
// in plaintext.
func buildCloudInitUserData(s Spec) (string, error) {
	if !safeUsernameRe.MatchString(s.Username) {
		return "", fmt.Errorf("unattend: username %q is not a safe POSIX username (must match %s); "+
			"it is emitted into a sudoers file and a shell command", s.Username, safeUsernameRe)
	}

	hash, err := generateSHA512Crypt(s.Password)
	if err != nil {
		return "", fmt.Errorf("unattend: hash password: %w", err)
	}

	pkgs := append([]string{"open-vm-tools", "cloud-init"}, s.ExtraPkgs...)

	ai := ciAutoinstall{
		Version: 1,
		Identity: ciIdentity{
			Hostname: s.Hostname,
			Username: s.Username,
			Password: hash,
		},
		SSH:      ciSSH{InstallServer: true},
		Packages: pkgs,
		Locale:   s.Locale,
		Keyboard: ciKeyboard{Layout: "us"},
		Timezone: s.TimeZone,
		EarlyCommands: []string{
			autoConfirmScript,
		},
		// Power off rather than reboot when the install finishes. This is the
		// provisioner's completion signal: the Ubuntu live-server ISO runs
		// open-vm-tools in the *installer* environment and reports Tools within
		// ~40 seconds of power-on, long before any OS exists on disk, so "Tools
		// are up" cannot mean "the install is done". A clean power-off can only
		// happen after curtin has finished writing the target system.
		Shutdown: "poweroff",
		LateCommands: []string{
			// Install the Crucible cloud.cfg into the target system.
			inTarget(writeFileScript("/etc/cloud/cloud.cfg.d/99-crucible.cfg", crucibleCloudCfg, "0644")),

			// Grant the install user passwordless sudo.
			//
			// crucibleCloudCfg's system_info.default_user.sudo does NOT cover
			// this: cloud-init only applies default_user when it CREATES the
			// user, and subiquity's identity block has already created it. The
			// user lands in group sudo, which Ubuntu's stock sudoers defines as
			// "%sudo ALL=(ALL:ALL) ALL" - password required.
			//
			// That breaks the template pipeline outright. generalizeScript()
			// runs "sudo cloud-init clean", "sudo truncate -s 0
			// /etc/machine-id" and "sudo rm -f /etc/ssh/ssh_host_*" over VMware
			// guest ops with no tty, under "set -e". Verified on the first real
			// ISO build: "sudo -n true" returned "sudo: a password is required".
			//
			// visudo -cf is not decoration: an invalid file in sudoers.d breaks
			// sudo for every user. Failing the install here is far better than
			// shipping a template nobody can escalate on.
			inTarget(writeFileScript(sudoersPath(s.Username),
				s.Username+" ALL=(ALL) NOPASSWD:ALL\n", "0440") +
				" && visudo -cf " + shellQuote(sudoersPath(s.Username))),

			// Recreate SSH host keys on first boot of a clone (see
			// crucibleSSHKeygenUnit). Enabled by hand rather than via
			// "systemctl enable" semantics that need a running systemd: curtin
			// runs in a chroot, so we create the wants symlink directly.
			inTarget(writeFileScript("/etc/systemd/system/"+sshKeygenUnitName,
				crucibleSSHKeygenUnit, "0644") +
				" && mkdir -p /etc/systemd/system/multi-user.target.wants" +
				" && ln -sf /etc/systemd/system/" + sshKeygenUnitName +
				" /etc/systemd/system/multi-user.target.wants/" + sshKeygenUnitName),
		},
	}

	if s.AptProxy != "" {
		ai.Apt = &ciApt{Proxy: s.AptProxy}
	}

	body, err := yaml.Marshal(ciDocument{Autoinstall: ai})
	if err != nil {
		return "", fmt.Errorf("unattend: marshal user-data: %w", err)
	}

	return "#cloud-config\n" + string(body), nil
}

// shellQuote wraps a string in single quotes, escaping embedded single quotes,
// so it can be safely used as a single POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildCIDataISO renders the CIDATA seed ISO for an Ubuntu autoinstall.
func buildCIDataISO(s Spec) (name string, data []byte, err error) {
	userData, err := buildCloudInitUserData(s)
	if err != nil {
		return "", nil, err
	}

	// meta-data may be empty but the file must exist. We include an
	// instance-id so cloud-init treats each build as a distinct instance.
	metaData := ""
	if s.Hostname != "" {
		metaData = fmt.Sprintf("instance-id: crucible-%s\nlocal-hostname: %s\n", s.Hostname, s.Hostname)
	}

	files := []isoFile{
		{Path: "user-data", Data: []byte(userData)},
		{Path: "meta-data", Data: []byte(metaData)},
	}

	iso, err := buildISO(files, cidataVolumeLabel)
	if err != nil {
		return "", nil, err
	}
	return "seed-cidata.iso", iso, nil
}
