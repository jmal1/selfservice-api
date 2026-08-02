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
	Version      int        `yaml:"version"`
	Identity     ciIdentity `yaml:"identity"`
	SSH          ciSSH      `yaml:"ssh"`
	Packages     []string   `yaml:"packages"`
	Locale       string     `yaml:"locale"`
	Keyboard     ciKeyboard `yaml:"keyboard"`
	Timezone     string     `yaml:"timezone"`
	Apt          *ciApt     `yaml:"apt,omitempty"`
	LateCommands []string   `yaml:"late-commands"`
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
