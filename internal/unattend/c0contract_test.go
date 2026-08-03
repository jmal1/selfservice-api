package unattend

import (
	"strings"
	"testing"
)

// The three guards in this file all exist because of defects observed on the
// FIRST real ISO build of a template (2026-08-03). Every one of them produced a
// green build, a healthy-looking template, and an unusable result -- none were
// reachable by any test that did not boot a real installer.

// TestBuildSeedISO_CIData_AptProxyIsScalar guards the shape of autoinstall.apt.
//
// Production symptom of the bug this replaces: the generator nested http_proxy
// and https_proxy UNDER apt.proxy. curtin's schema has them as sibling scalars,
// so it stringified the mapping and wrote
//
//	Acquire::http::Proxy "{'http_proxy': 'http://10.10.30.20:3142', ...}";
//
// into /etc/apt/apt.conf.d/90curtin-aptproxy on the installed system. That is a
// syntactically valid apt.conf line containing a garbage proxy URL, so apt did
// not error at parse time -- it failed at every fetch, on the template and on
// every student clone made from it. The install itself still "succeeded".
func TestBuildSeedISO_CIData_AptProxyIsScalar(t *testing.T) {
	const proxy = "http://10.10.30.20:3142"

	_, ud := cidataUserData(t, Spec{
		Mode:     ModeCloudInitCIData,
		Hostname: "ubuntu-lab",
		Password: testPlaintextPassword,
		AptProxy: proxy,
	})

	ai := autoinstallBlock(t, ud)
	apt, ok := ai["apt"].(map[string]interface{})
	if !ok {
		t.Fatalf("autoinstall.apt is %T, want a mapping", ai["apt"])
	}

	got, ok := apt["proxy"]
	if !ok {
		t.Fatal("autoinstall.apt has no proxy key; the apt cache would be bypassed entirely")
	}
	str, ok := got.(string)
	if !ok {
		t.Fatalf("autoinstall.apt.proxy is %T (%v), want a plain string. curtin does not "+
			"validate this: it stringifies whatever it is given, so a mapping here becomes a "+
			"literal Python dict repr in /etc/apt/apt.conf.d/90curtin-aptproxy and breaks apt "+
			"on the template and every clone", got, got)
	}
	if str != proxy {
		t.Errorf("autoinstall.apt.proxy = %q, want %q", str, proxy)
	}

	// https_proxy must NOT be pointed at apt-cacher-ng: it proxies http and
	// would break https repositories rather than caching them.
	if _, bad := apt["https_proxy"]; bad {
		t.Error("autoinstall.apt.https_proxy is set; apt-cacher-ng is an http cache and " +
			"routing https through it breaks https repositories")
	}

	// And with no proxy configured, the key must be absent rather than empty.
	_, udNoProxy := cidataUserData(t, Spec{
		Mode:     ModeCloudInitCIData,
		Hostname: "ubuntu-lab",
		Password: testPlaintextPassword,
	})
	if _, present := autoinstallBlock(t, udNoProxy)["apt"]; present {
		t.Error("autoinstall.apt is present with no AptProxy configured; " +
			"an empty proxy string makes apt fail closed")
	}
}

// TestBuildSeedISO_CIData_GrantsPasswordlessSudo guards the template pipeline's
// ability to run ANY privileged command in the guest.
//
// crucibleCloudCfg's system_info.default_user.sudo does not cover this, which is
// exactly why the bug shipped: the config looks like it grants NOPASSWD sudo,
// but cloud-init only applies default_user when it CREATES the user, and
// subiquity's identity block created it first. Observed on the real build:
//
//	$ sudo -n true
//	sudo: a password is required
//
// generalizeScript() runs "sudo cloud-init clean", "sudo truncate -s 0
// /etc/machine-id" and "sudo rm -f /etc/ssh/ssh_host_*" through VMware guest ops
// with no tty and under "set -e", so generalize could never have succeeded.
func TestBuildSeedISO_CIData_GrantsPasswordlessSudo(t *testing.T) {
	for _, user := range []string{"student", "ops"} {
		t.Run(user, func(t *testing.T) {
			_, ud := cidataUserData(t, Spec{
				Mode:     ModeCloudInitCIData,
				Hostname: "ubuntu-lab",
				Username: user,
				Password: testPlaintextPassword,
			})

			late := strings.Join(lateCommands(t, ud), "\n")

			want := user + " ALL=(ALL) NOPASSWD:ALL"
			if !strings.Contains(late, want) {
				t.Fatalf("late-commands never grant passwordless sudo (%q missing).\n"+
					"Without it every sudo in generalizeScript() blocks on a password prompt "+
					"over guest ops and generalize fails.\nlate-commands:\n%s", want, late)
			}
			if !strings.Contains(late, "/etc/sudoers.d/90-crucible-"+user) {
				t.Errorf("sudo grant is not written to a sudoers.d drop-in; editing /etc/sudoers "+
					"in place is not safely idempotent.\nlate-commands:\n%s", late)
			}
			// sudo silently IGNORES a drop-in that is group- or world-writable,
			// which would reproduce the original bug with no error anywhere.
			if !strings.Contains(late, "chmod 0440") {
				t.Errorf("sudoers drop-in is not chmod 0440; sudo ignores drop-ins with " +
					"unsafe modes and does not say so")
			}
			// An invalid sudoers file breaks sudo for EVERY user, including
			// root recovery paths. Fail the install instead of shipping that.
			if !strings.Contains(late, "visudo -cf") {
				t.Errorf("sudoers drop-in is not validated with visudo -cf; a malformed file " +
					"disables sudo for every user on the template")
			}
		})
	}
}

// TestBuildSeedISO_CIData_InstallsHostKeyRegenUnit guards acceptance test C4-5.
//
// generalizeScript() deletes /etc/ssh/ssh_host_* so each clone gets its own host
// identity. Ubuntu 24.04 ships nothing that recreates them -- verified on the
// real build, where "systemctl is-enabled ssh-keygen.service" returned
// not-found and ssh is socket-activated.
//
// This failure is invisible on one clone and only shows up on the second, which
// is precisely the class of bug C4-5 exists to catch.
func TestBuildSeedISO_CIData_InstallsHostKeyRegenUnit(t *testing.T) {
	_, ud := cidataUserData(t, Spec{
		Mode:     ModeCloudInitCIData,
		Hostname: "ubuntu-lab",
		Password: testPlaintextPassword,
	})

	late := strings.Join(lateCommands(t, ud), "\n")

	if !strings.Contains(late, "ssh-keygen -A") {
		t.Fatalf("late-commands never install an ssh-keygen -A unit. After generalize deletes "+
			"/etc/ssh/ssh_host_*, nothing on Ubuntu 24.04 puts them back before ssh.socket "+
			"accepts a connection.\nlate-commands:\n%s", late)
	}
	if !strings.Contains(late, "/etc/systemd/system/"+sshKeygenUnitName) {
		t.Errorf("host-key regen unit is not written to /etc/systemd/system/%s\nlate-commands:\n%s",
			sshKeygenUnitName, late)
	}
	// curtin runs in a chroot with no running systemd, so "systemctl enable"
	// is unreliable there; the wants symlink must be created directly.
	if !strings.Contains(late, "multi-user.target.wants/"+sshKeygenUnitName) {
		t.Errorf("host-key regen unit is written but never enabled (no multi-user.target.wants "+
			"symlink), so it would never run.\nlate-commands:\n%s", late)
	}
	// Must not rotate keys on an already-provisioned pod.
	if !strings.Contains(crucibleSSHKeygenUnit, "ConditionPathExists=!/etc/ssh/ssh_host_ed25519_key") {
		t.Error("regen unit lacks the ConditionPathExists guard; it would run on every boot " +
			"and could rotate a live pod's host key")
	}
	// Must beat socket activation, or a clone can refuse SSH until cloud-init
	// happens to catch up. Ordering before ssh.service is what actually
	// matters: ssh.socket only binds the port, ssh.service execs sshd and
	// reads the host keys.
	if !strings.Contains(crucibleSSHKeygenUnit, "Before=ssh.service") {
		t.Error("regen unit does not order itself before ssh.service; " +
			"a clone could accept a connection before its host keys exist")
	}
}

// TestSSHKeygenUnit_NoOrderingCycleWithSocketsTarget guards the defect that took
// SSH down on the first real ISO build.
//
// The unit is pulled in by multi-user.target, so systemd's default dependencies
// give it After=basic.target, and basic.target is After sockets.target. Since
// ssh.socket is Before=sockets.target, ANY "Before=ssh.socket" (or
// "Before=sockets.target") on this unit closes an ordering cycle. systemd breaks
// such a cycle by deleting a job, and on the real build it deleted
// ssh.socket/start:
//
//	sockets.target: Found ordering cycle on ssh.socket/start
//	sockets.target: Job ssh.socket/start deleted to break ordering cycle
//
// Result: nothing listened on port 22, on the template AND on every student
// clone made from it. The ConditionPathExists guard does not save you - systemd
// resolves ordering cycles before evaluating conditions.
func TestSSHKeygenUnit_NoOrderingCycleWithSocketsTarget(t *testing.T) {
	installTarget := ""
	var beforeUnits []string
	for _, line := range strings.Split(crucibleSSHKeygenUnit, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Before="):
			beforeUnits = append(beforeUnits, strings.Fields(strings.TrimPrefix(line, "Before="))...)
		case strings.HasPrefix(line, "WantedBy="):
			installTarget = strings.TrimSpace(strings.TrimPrefix(line, "WantedBy="))
		}
	}

	// Assert the premise, so this test cannot pass vacuously if the unit is
	// later re-attached to an early target where Before=ssh.socket would be
	// legitimate.
	if installTarget != "multi-user.target" {
		t.Fatalf("this guard assumes WantedBy=multi-user.target (which implies After=basic.target, "+
			"which is After sockets.target); got WantedBy=%q. Re-derive the cycle before "+
			"relaxing the check below.", installTarget)
	}
	if len(beforeUnits) == 0 {
		t.Fatal("no Before= line found in the regen unit; the guard below would be hollow")
	}

	for _, u := range beforeUnits {
		if u == "ssh.socket" || u == "sockets.target" {
			t.Errorf("regen unit declares Before=%s while WantedBy=multi-user.target. "+
				"That is a systemd ordering cycle: multi-user.target -> After basic.target "+
				"-> After sockets.target -> After ssh.socket -> After this unit. systemd "+
				"breaks it by DELETING ssh.socket/start, so port 22 never binds on the "+
				"template or on any clone of it. Order before ssh.service instead.", u)
		}
	}
}

// TestBuildSeedISO_CIData_RejectsUnsafeUsername stops a hostile or typo'd
// username from being interpolated into a sudoers file and a shell command.
func TestBuildSeedISO_CIData_RejectsUnsafeUsername(t *testing.T) {
	for _, bad := range []string{
		"stu dent",
		"student; rm -rf /",
		"student'",
		"../root",
		"ALL",
		"Student",
		strings.Repeat("a", 40),
	} {
		t.Run(bad, func(t *testing.T) {
			_, _, err := BuildSeedISO(Spec{
				Mode:     ModeCloudInitCIData,
				Hostname: "ubuntu-lab",
				Username: bad,
				Password: testPlaintextPassword,
			})
			if err == nil {
				t.Fatalf("BuildSeedISO accepted username %q; it is written verbatim into "+
					"/etc/sudoers.d and into an sh -c command", bad)
			}
		})
	}
}

// lateCommands decodes autoinstall.late-commands from generated user-data.
func lateCommands(t *testing.T, ud []byte) []string {
	t.Helper()
	ai := autoinstallBlock(t, ud)
	raw, ok := ai["late-commands"]
	if !ok {
		t.Fatal("autoinstall has no late-commands block")
	}
	return stringSlice(t, raw, "late-commands")
}
