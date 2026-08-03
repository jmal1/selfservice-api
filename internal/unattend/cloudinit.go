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
	"strings"

	"gopkg.in/yaml.v3"
)

// cidataVolumeLabel is the exact volume label cloud-init's NoCloud datasource
// looks for. Do not change this.
const cidataVolumeLabel = "CIDATA"

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

type ciAptProxyPair struct {
	HTTPProxy  string `yaml:"http_proxy"`
	HTTPSProxy string `yaml:"https_proxy"`
}

type ciApt struct {
	Proxy *ciAptProxyPair `yaml:"proxy,omitempty"`
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
			"curtin in-target --target=/target -- sh -c " +
				shellQuote("mkdir -p /etc/cloud/cloud.cfg.d && printf '%s' "+
					shellQuote(crucibleCloudCfg)+" > /etc/cloud/cloud.cfg.d/99-crucible.cfg"),
		},
	}

	if s.AptProxy != "" {
		ai.Apt = &ciApt{Proxy: &ciAptProxyPair{
			HTTPProxy:  s.AptProxy,
			HTTPSProxy: s.AptProxy,
		}}
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
