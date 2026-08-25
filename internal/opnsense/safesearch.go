package opnsense

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	sourceScopedSafeSearchMarker = "# crucible:source-scoped-safesearch:v1"
	sourceScopedSafeSearchView   = "crucible-student-safesearch"

	sourceScopedSafeSearchPath       = "/usr/local/etc/unbound.opnsense.d/crucible-student-safesearch.conf"
	sourceScopedSafeSearchStagedPath = "/var/unbound/etc/crucible-student-safesearch.conf"
	maxSafeSearchFragmentBytes       = 256 * 1024
)

// The list matches OPNsense 26.1's generated Unbound safesearch.conf template.
const googleSafeSearchDomains = `
google.com google.ad google.ae google.com.af google.com.ag google.com.ai google.al google.am google.co.ao
google.com.ar google.as google.at google.com.au google.az google.ba google.com.bd google.be google.bf
google.bg google.com.bh google.bi google.bj google.com.bn google.com.bo google.com.br google.bs google.bt
google.co.bw google.by google.com.bz google.ca google.cd google.cf google.cg google.ch google.ci google.co.ck
google.cl google.cm google.cn google.com.co google.co.cr google.com.cu google.cv google.com.cy google.cz
google.de google.dj google.dk google.dm google.com.do google.dz google.com.ec google.ee google.com.eg google.es
google.com.et google.fi google.com.fj google.fm google.fr google.ga google.ge google.gg google.com.gh
google.com.gi google.gl google.gm google.gr google.com.gt google.gy google.com.hk google.hn google.hr google.ht
google.hu google.co.id google.ie google.co.il google.im google.co.in google.iq google.is google.it google.je
google.com.jm google.jo google.co.jp google.co.ke google.com.kh google.ki google.kg google.co.kr google.com.kw
google.kz google.la google.com.lb google.li google.lk google.co.ls google.lt google.lu google.lv google.com.ly
google.co.ma google.md google.me google.mg google.mk google.ml google.com.mm google.mn google.ms google.com.mt
google.mu google.mv google.mw google.com.mx google.com.my google.co.mz google.com.na google.com.ng google.com.ni
google.ne google.nl google.no google.com.np google.nr google.nu google.co.nz google.com.om google.com.pa
google.com.pe google.com.pg google.com.ph google.com.pk google.pl google.pn google.com.pr google.ps google.pt
google.com.py google.com.qa google.ro google.ru google.rw google.com.sa google.com.sb google.sc google.se
google.com.sg google.sh google.si google.sk google.com.sl google.sn google.so google.sm google.sr google.st
google.com.sv google.td google.tg google.co.th google.com.tj google.tl google.tm google.tn google.to google.com.tr
google.tt google.com.tw google.co.tz google.com.ua google.co.ug google.co.uk google.com.uy google.co.uz
google.com.vc google.co.ve google.vg google.co.vi google.com.vn google.vu google.ws google.rs google.co.za
google.co.zm google.co.zw google.cat
`

type sourceScopedSafeSearchRemote interface {
	ReadFile(context.Context, string) ([]byte, bool, error)
	WriteFileAtomic(context.Context, string, []byte) error
	CopyFileAtomic(context.Context, string, string) error
	RemoveFile(context.Context, string) error
	CheckConflicts(context.Context, string) error
	CheckUnbound(context.Context) error
	ReconfigureUnbound(context.Context) error
}

type sourceScopedSafeSearchSnapshot struct {
	content []byte
	exists  bool
}

type sourceScopedSafeSearchManager struct {
	remote sourceScopedSafeSearchRemote
}

// ConfigureSourceScopedSafeSearch installs the dedicated student-only Unbound
// view transactionally. It is intentionally not called by content-filter
// reconciliation yet.
func (s *SSHClient) ConfigureSourceScopedSafeSearch(ctx context.Context, sourceNetwork string) error {
	fragment, canonicalSource, err := renderSourceScopedSafeSearch(sourceNetwork)
	if err != nil {
		return err
	}
	client, err := s.dial()
	if err != nil {
		return fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close()

	manager := sourceScopedSafeSearchManager{
		remote: &sshSourceScopedSafeSearchRemote{client: client},
	}
	return manager.configure(ctx, canonicalSource, fragment)
}

// ConfigureSourceScopedSafeSearch exposes the owner through the combined
// OPNsense client without wiring it into the read-only reconciler.
func (c *Client) ConfigureSourceScopedSafeSearch(ctx context.Context, sourceNetwork string) error {
	sshClient, err := NewSSHClient(c.config, c.logger)
	if err != nil {
		return fmt.Errorf("configure source-scoped SafeSearch: %w", err)
	}
	return sshClient.ConfigureSourceScopedSafeSearch(ctx, sourceNetwork)
}

func (m sourceScopedSafeSearchManager) configure(ctx context.Context, sourceNetwork string, fragment []byte) error {
	previous, err := m.readOwnedSnapshot(ctx, sourceScopedSafeSearchPath)
	if err != nil {
		return fmt.Errorf("read previous owned SafeSearch fragment: %w", err)
	}
	previousStaged, err := m.readOwnedSnapshot(ctx, sourceScopedSafeSearchStagedPath)
	if err != nil {
		return fmt.Errorf("read previous staged SafeSearch fragment: %w", err)
	}
	if err := m.remote.CheckConflicts(ctx, sourceNetwork); err != nil {
		return fmt.Errorf("check Unbound fragment conflicts: %w", err)
	}

	if err := m.remote.WriteFileAtomic(ctx, sourceScopedSafeSearchPath, fragment); err != nil {
		return m.rollback(ctx, previous, previousStaged, fmt.Errorf("write owned SafeSearch fragment: %w", err))
	}
	if err := m.remote.CopyFileAtomic(ctx, sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath); err != nil {
		return m.rollback(ctx, previous, previousStaged, fmt.Errorf("stage owned SafeSearch fragment: %w", err))
	}
	if err := m.remote.CheckUnbound(ctx); err != nil {
		return m.rollback(ctx, previous, previousStaged, fmt.Errorf("validate staged Unbound configuration: %w", err))
	}
	if err := m.remote.ReconfigureUnbound(ctx); err != nil {
		return m.rollback(ctx, previous, previousStaged, fmt.Errorf("activate staged Unbound configuration: %w", err))
	}
	for _, path := range []string{sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath} {
		content, exists, err := m.remote.ReadFile(ctx, path)
		if err != nil {
			return m.rollback(ctx, previous, previousStaged, fmt.Errorf("verify activated fragment %s: %w", path, err))
		}
		if !exists || !bytes.Equal(content, fragment) {
			return m.rollback(ctx, previous, previousStaged,
				fmt.Errorf("verify activated fragment %s: exact content mismatch", path))
		}
	}
	return nil
}

func (m sourceScopedSafeSearchManager) readOwnedSnapshot(ctx context.Context, path string) (sourceScopedSafeSearchSnapshot, error) {
	content, exists, err := m.remote.ReadFile(ctx, path)
	if err != nil {
		return sourceScopedSafeSearchSnapshot{}, err
	}
	if exists && !bytes.HasPrefix(content, []byte(sourceScopedSafeSearchMarker+"\n")) {
		return sourceScopedSafeSearchSnapshot{}, fmt.Errorf("refusing to overwrite unowned file %s", path)
	}
	return sourceScopedSafeSearchSnapshot{content: content, exists: exists}, nil
}

func (m sourceScopedSafeSearchManager) rollback(
	ctx context.Context,
	previous sourceScopedSafeSearchSnapshot,
	previousStaged sourceScopedSafeSearchSnapshot,
	cause error,
) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()

	var rollbackErrs []error
	restore := func(path string, snapshot sourceScopedSafeSearchSnapshot) {
		var err error
		if snapshot.exists {
			err = m.remote.WriteFileAtomic(rollbackCtx, path, snapshot.content)
		} else {
			err = m.remote.RemoveFile(rollbackCtx, path)
		}
		if err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("restore %s: %w", path, err))
		}
	}
	restore(sourceScopedSafeSearchPath, previous)
	restore(sourceScopedSafeSearchStagedPath, previousStaged)
	if err := m.remote.CheckUnbound(rollbackCtx); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("validate rolled-back Unbound configuration: %w", err))
	}
	if err := m.remote.ReconfigureUnbound(rollbackCtx); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("activate rolled-back Unbound configuration: %w", err))
	}
	if len(rollbackErrs) == 0 {
		return fmt.Errorf("%w; rollback completed", cause)
	}
	return errors.Join(cause, fmt.Errorf("SafeSearch rollback incomplete: %w", errors.Join(rollbackErrs...)))
}

func renderSourceScopedSafeSearch(sourceNetwork string) ([]byte, string, error) {
	source, err := validateStudentSourceNetwork(sourceNetwork)
	if err != nil {
		return nil, "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", sourceScopedSafeSearchMarker)
	b.WriteString("# Dedicated OPNsense 26.1 Unbound view; never enable the generated global SafeSearch switch.\n")
	b.WriteString("server:\n")
	fmt.Fprintf(&b, "    access-control-view: %s %s\n\n", source, sourceScopedSafeSearchView)
	b.WriteString("view:\n")
	fmt.Fprintf(&b, "    name: %q\n", sourceScopedSafeSearchView)
	b.WriteString("    view-first: yes\n")
	for _, domain := range strings.Fields(googleSafeSearchDomains) {
		writeSafeSearchMapping(&b, "www."+domain, "forcesafesearch.google.com")
	}
	writeSafeSearchMapping(&b, "duckduckgo.com", "safe.duckduckgo.com")
	writeSafeSearchMapping(&b, "duck.com", "safe.duckduckgo.com")
	b.WriteString("    local-zone: \"external-content.duckduckgo.com\" always_transparent\n")
	writeSafeSearchMapping(&b, "bing.com", "strict.bing.com")
	for _, domain := range []string{
		"www.youtube.com",
		"m.youtube.com",
		"youtubei.googleapis.com",
		"youtube.googleapis.com",
		"www.youtube-nocookie.com",
	} {
		writeSafeSearchMapping(&b, domain, "restrictmoderate.youtube.com")
	}
	writeSafeSearchMapping(&b, "pixabay.com", "safesearch.pixabay.com")
	writeSafeSearchMapping(&b, "qwant.com", "safeapi.qwant.com")
	return []byte(b.String()), source, nil
}

func writeSafeSearchMapping(b *strings.Builder, domain, target string) {
	fmt.Fprintf(b, "    local-zone: %q redirect\n", domain)
	fmt.Fprintf(b, "    local-data: %q\n", domain+" CNAME "+target)
}

func validateStudentSourceNetwork(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed != value || trimmed == "" {
		return "", fmt.Errorf("student source network must be a non-empty canonical IPv4 CIDR")
	}
	prefix, err := netip.ParsePrefix(trimmed)
	if err != nil || !prefix.Addr().Is4() {
		return "", fmt.Errorf("student source network %q must be a canonical IPv4 CIDR", value)
	}
	masked := prefix.Masked()
	if masked.String() != trimmed {
		return "", fmt.Errorf("student source network %q has host bits or non-canonical notation", value)
	}
	if !containedInRFC1918(masked) {
		return "", fmt.Errorf("student source network %q must be a private unicast IPv4 network", value)
	}
	return masked.String(), nil
}

func containedInRFC1918(prefix netip.Prefix) bool {
	for _, raw := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		private := netip.MustParsePrefix(raw)
		if prefix.Bits() >= private.Bits() && private.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

type sshSourceScopedSafeSearchRemote struct {
	client *ssh.Client
}

func (r *sshSourceScopedSafeSearchRemote) ReadFile(ctx context.Context, path string) ([]byte, bool, error) {
	if err := validateSafeSearchManagedPath(path); err != nil {
		return nil, false, err
	}
	command := fmt.Sprintf(
		"/bin/sh -c 'if [ ! -e %s ]; then printf \"CRUCIBLE_ABSENT\\\\n\"; exit 0; fi; "+
			"if [ -L %s ] || [ ! -f %s ]; then printf \"managed path is not a regular file\\\\n\" >&2; exit 1; fi; "+
			"size=$(/usr/bin/stat -f %%z %s) || exit 1; "+
			"[ \"$size\" -le %d ] || { printf \"managed fragment exceeds size limit\\\\n\" >&2; exit 1; }; "+
			"printf \"CRUCIBLE_PRESENT\\\\n\"; /usr/bin/base64 < %s'",
		path, path, path, path, maxSafeSearchFragmentBytes, path,
	)
	output, err := r.run(ctx, command)
	if err != nil {
		return nil, false, err
	}
	header, encoded, found := strings.Cut(output, "\n")
	if !found {
		return nil, false, fmt.Errorf("malformed remote file response")
	}
	switch header {
	case "CRUCIBLE_ABSENT":
		return nil, false, nil
	case "CRUCIBLE_PRESENT":
		content, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, false, fmt.Errorf("decode remote file: %w", err)
		}
		return content, true, nil
	default:
		return nil, false, fmt.Errorf("unexpected remote file response %q", header)
	}
}

func (r *sshSourceScopedSafeSearchRemote) WriteFileAtomic(ctx context.Context, path string, content []byte) error {
	if err := validateSafeSearchManagedPath(path); err != nil {
		return err
	}
	if len(content) > maxSafeSearchFragmentBytes {
		return fmt.Errorf("SafeSearch fragment is %d bytes, limit %d", len(content), maxSafeSearchFragmentBytes)
	}
	encoded := base64.StdEncoding.EncodeToString(content)
	tmp := path + ".crucible-tmp"
	command := fmt.Sprintf(
		"/bin/sh -c 'umask 077; tmp=%s; rm -f \"$tmp\"; printf %%s %s | /usr/bin/base64 -d > \"$tmp\" && chmod 0644 \"$tmp\" && mv -f \"$tmp\" %s; rc=$?; rm -f \"$tmp\"; exit $rc'",
		tmp, encoded, path,
	)
	_, err := r.run(ctx, command)
	return err
}

func (r *sshSourceScopedSafeSearchRemote) CopyFileAtomic(ctx context.Context, source, destination string) error {
	if err := validateSafeSearchManagedPath(source); err != nil {
		return err
	}
	if err := validateSafeSearchManagedPath(destination); err != nil {
		return err
	}
	tmp := destination + ".crucible-tmp"
	command := fmt.Sprintf(
		"/bin/sh -c 'umask 077; tmp=%s; rm -f \"$tmp\"; cp %s \"$tmp\" && chmod 0644 \"$tmp\" && mv -f \"$tmp\" %s; rc=$?; rm -f \"$tmp\"; exit $rc'",
		tmp, source, destination,
	)
	_, err := r.run(ctx, command)
	return err
}

func (r *sshSourceScopedSafeSearchRemote) RemoveFile(ctx context.Context, path string) error {
	if err := validateSafeSearchManagedPath(path); err != nil {
		return err
	}
	_, err := r.run(ctx, "/bin/sh -c 'rm -f "+path+" "+path+".crucible-tmp'")
	return err
}

func (r *sshSourceScopedSafeSearchRemote) CheckConflicts(ctx context.Context, sourceNetwork string) error {
	if _, err := validateStudentSourceNetwork(sourceNetwork); err != nil {
		return err
	}
	sourcePattern := regexp.QuoteMeta(sourceNetwork)
	command := fmt.Sprintf(
		"/bin/sh -c 'for file in /usr/local/etc/unbound.opnsense.d/*.conf; do "+
			"[ -f \"$file\" ] || continue; [ \"$file\" = %s ] && continue; "+
			"if grep -F -q -- %q \"$file\" || grep -E -q -- %q \"$file\"; then "+
			"printf \"conflicting Unbound fragment: %%s\\\\n\" \"$file\" >&2; exit 1; fi; done'",
		sourceScopedSafeSearchPath,
		sourceScopedSafeSearchView,
		`access-control-view:[[:space:]]*"?`+sourcePattern+`([[:space:]]|")`,
	)
	_, err := r.run(ctx, command)
	return err
}

func (r *sshSourceScopedSafeSearchRemote) CheckUnbound(ctx context.Context) error {
	output, err := r.run(ctx, "/usr/local/sbin/configctl unbound check")
	if err != nil {
		return err
	}
	if err := validateUnboundCheckOutput(output); err != nil {
		return fmt.Errorf("configctl unbound check: %w", err)
	}
	output, err = r.run(ctx, "/usr/local/sbin/unbound-checkconf /var/unbound/unbound.conf")
	if err != nil {
		return err
	}
	if err := validateUnboundCheckOutput(output); err != nil {
		return fmt.Errorf("direct unbound-checkconf: %w", err)
	}
	return nil
}

func (r *sshSourceScopedSafeSearchRemote) ReconfigureUnbound(ctx context.Context) error {
	output, err := r.run(ctx, "/usr/local/sbin/configctl unbound restart")
	if err != nil {
		return err
	}
	if err := validateUnboundRestartOutput(output); err != nil {
		return err
	}
	output, err = r.run(ctx, "/usr/local/sbin/configctl unbound status")
	if err != nil {
		return err
	}
	return validateUnboundStatusOutput(output)
}

func validateUnboundCheckOutput(output string) error {
	const expected = "no errors in /var/unbound/unbound.conf"
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		return fmt.Errorf("unexpected validation output %q", strings.TrimSpace(output))
	}
	line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[0]), "unbound-checkconf:"))
	if line != expected {
		return fmt.Errorf("unexpected validation output %q", strings.TrimSpace(output))
	}
	return nil
}

func validateUnboundRestartOutput(output string) error {
	trimmed := strings.TrimSpace(output)
	if trimmed != "" && !strings.EqualFold(trimmed, "ok") {
		return fmt.Errorf("configctl unbound restart returned unexpected output %q", trimmed)
	}
	return nil
}

func validateUnboundStatusOutput(output string) error {
	trimmed := strings.TrimSpace(output)
	if !strings.Contains(strings.ToLower(trimmed), "is running") {
		return fmt.Errorf("Unbound did not report running after restart: %q", trimmed)
	}
	return nil
}

func (r *sshSourceScopedSafeSearchRemote) run(ctx context.Context, command string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	session, err := r.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("create SSH session: %w", err)
	}
	defer session.Close()

	var output bytes.Buffer
	session.Stdout = &output
	session.Stderr = &output
	if err := session.Start(command); err != nil {
		return output.String(), fmt.Errorf("start SSH command: %w (output: %s)", err, output.String())
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return output.String(), fmt.Errorf("SSH command failed: %w (output: %s)", err, output.String())
		}
		return output.String(), nil
	case <-ctx.Done():
		_ = session.Close()
		<-done
		return output.String(), ctx.Err()
	}
}

func validateSafeSearchManagedPath(path string) error {
	switch path {
	case sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath:
		return nil
	default:
		return fmt.Errorf("refusing unmanaged SafeSearch path %q", path)
	}
}
