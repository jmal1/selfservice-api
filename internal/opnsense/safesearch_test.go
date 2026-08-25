package opnsense

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRenderSourceScopedSafeSearchMatchesOPNsense26Mappings(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if source != "10.100.0.0/16" {
		t.Fatalf("source = %q", source)
	}
	text := string(fragment)
	for _, required := range []string{
		sourceScopedSafeSearchMarker,
		"access-control-view: 10.100.0.0/16 " + sourceScopedSafeSearchView,
		`name: "` + sourceScopedSafeSearchView + `"`,
		"view-first: yes",
		`www.google.com CNAME forcesafesearch.google.com`,
		`duckduckgo.com CNAME safe.duckduckgo.com`,
		`duck.com CNAME safe.duckduckgo.com`,
		`external-content.duckduckgo.com" always_transparent`,
		`bing.com CNAME strict.bing.com`,
		`www.youtube.com CNAME restrictmoderate.youtube.com`,
		`www.youtube-nocookie.com CNAME restrictmoderate.youtube.com`,
		`pixabay.com CNAME safesearch.pixabay.com`,
		`qwant.com CNAME safeapi.qwant.com`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("fragment omitted %q", required)
		}
	}
	if got, want := strings.Count(text, "CNAME forcesafesearch.google.com"), len(strings.Fields(googleSafeSearchDomains)); got != want {
		t.Errorf("Google mapping count = %d, want %d", got, want)
	}
	if strings.Contains(text, "safesearch.conf") || strings.Count(text, "\nview:\n") != 1 {
		t.Fatalf("fragment edits a global file or renders multiple views:\n%s", text)
	}
}

func TestValidateStudentSourceNetworkRejectsUnsafeOrNonCanonicalValues(t *testing.T) {
	for _, value := range []string{
		"",
		" 10.100.0.0/16",
		"10.100.1.1/16",
		"10.100.0.0/16; id",
		"10.100.0.0/16'$(id)",
		"2001:db8::/32",
		"8.8.8.0/24",
		"10.0.0.0/7",
		"192.168.0.0/14",
		"127.0.0.0/8",
		"0.0.0.0/0",
	} {
		if _, err := validateStudentSourceNetwork(value); err == nil {
			t.Errorf("accepted unsafe source network %q", value)
		}
	}
	for _, value := range []string{"10.100.0.0/16", "172.16.10.0/24", "192.168.1.10/32"} {
		if got, err := validateStudentSourceNetwork(value); err != nil || got != value {
			t.Errorf("validateStudentSourceNetwork(%q) = %q, %v", value, got, err)
		}
	}
}

func TestUnboundCommandOutputValidationFailsClosed(t *testing.T) {
	for _, output := range []string{
		"",
		"fatal error: duplicate local-zone",
		"Script action failed",
		"no errors in /var/unbound/unbound.conf\nwarning: ignored",
		"no errors in /tmp/other.conf",
	} {
		if err := validateUnboundCheckOutput(output); err == nil {
			t.Errorf("accepted sabotaged configctl output %q", output)
		}
	}
	for _, output := range []string{
		"no errors in /var/unbound/unbound.conf",
		"unbound-checkconf: no errors in /var/unbound/unbound.conf\n",
	} {
		if err := validateUnboundCheckOutput(output); err != nil {
			t.Errorf("rejected valid unbound-checkconf output %q: %v", output, err)
		}
	}
	for _, output := range []string{"Script action failed", "unknown action", "fatal error"} {
		if err := validateUnboundRestartOutput(output); err == nil {
			t.Errorf("accepted sabotaged restart output %q", output)
		}
	}
	for _, output := range []string{"", "\n", "OK", " ok\n"} {
		if err := validateUnboundRestartOutput(output); err != nil {
			t.Errorf("rejected valid restart output %q: %v", output, err)
		}
	}
	for _, output := range []string{"", "unbound is not running", "failed"} {
		if err := validateUnboundStatusOutput(output); err == nil {
			t.Errorf("accepted sabotaged status output %q", output)
		}
	}
	if err := validateUnboundStatusOutput("unbound is running as pid 1234"); err != nil {
		t.Errorf("rejected running status: %v", err)
	}
}

func TestSafeSearchManagedPathRejectsArbitraryPaths(t *testing.T) {
	for _, path := range []string{
		"/usr/local/etc/unbound.opnsense.d/manual.conf",
		sourceScopedSafeSearchPath + ";rm -rf /",
		"../../etc/passwd",
		"",
	} {
		if err := validateSafeSearchManagedPath(path); err == nil {
			t.Errorf("accepted unmanaged path %q", path)
		}
	}
	for _, path := range []string{sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath} {
		if err := validateSafeSearchManagedPath(path); err != nil {
			t.Errorf("rejected managed path %q: %v", path, err)
		}
	}
}

func TestSourceScopedSafeSearchManagerSuccess(t *testing.T) {
	remote := newFakeSafeSearchRemote()
	manager := sourceScopedSafeSearchManager{remote: remote}
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.configure(context.Background(), source, fragment); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath} {
		if !bytes.Equal(remote.files[path], fragment) {
			t.Errorf("%s did not contain exact rendered fragment", path)
		}
	}
	if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; exists {
		t.Fatal("successful transaction retained rollback state")
	}
	if got := strings.Join(remote.calls, ","); got != "read,read,read,conflicts,write,write,copy,check,reconfigure,read,read,write,remove" {
		t.Fatalf("transaction calls = %s", got)
	}
}

func TestSourceScopedSafeSearchManagerRollsBackEveryForwardBoundary(t *testing.T) {
	previous := []byte(sourceScopedSafeSearchMarker + "\nprevious persistent\n")
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}

	for failCall := 6; failCall <= 12; failCall++ {
		t.Run(fmt.Sprintf("call-%d", failCall), func(t *testing.T) {
			remote := newFakeSafeSearchRemote()
			remote.files[sourceScopedSafeSearchPath] = bytes.Clone(previous)
			remote.files[sourceScopedSafeSearchStagedPath] = bytes.Clone(previous)
			remote.failCalls[failCall] = true

			err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
			if err == nil || !strings.Contains(err.Error(), "rollback completed") {
				t.Fatalf("configure error = %v, want completed rollback", err)
			}
			if !bytes.Equal(remote.files[sourceScopedSafeSearchPath], previous) {
				t.Errorf("persistent fragment not restored after call %d", failCall)
			}
			if !bytes.Equal(remote.files[sourceScopedSafeSearchStagedPath], previous) {
				t.Errorf("staged fragment not restored after call %d", failCall)
			}
			if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; exists {
				t.Errorf("completed rollback retained backup after call %d", failCall)
			}
		})
	}
}

func TestSourceScopedSafeSearchManagerPreflightFailuresDoNotMutate(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	for failCall := 1; failCall <= 5; failCall++ {
		t.Run(fmt.Sprintf("call-%d", failCall), func(t *testing.T) {
			remote := newFakeSafeSearchRemote()
			remote.failCalls[failCall] = true
			if err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment); err == nil {
				t.Fatal("configure succeeded despite injected preflight failure")
			}
			if _, exists := remote.files[sourceScopedSafeSearchPath]; exists {
				t.Fatal("preflight failure created persistent fragment")
			}
			if _, exists := remote.files[sourceScopedSafeSearchStagedPath]; exists {
				t.Fatal("preflight failure created staged fragment")
			}
			if failCall <= 4 {
				if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; exists {
					t.Fatalf("preflight failure retained backup: %v", remote.calls)
				}
			} else if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; exists {
				t.Fatalf("failed backup write changed remote state: %v", remote.calls)
			}
		})
	}
}

func TestSourceScopedSafeSearchManagerRemovesNewFragmentsOnFailure(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	remote := newFakeSafeSearchRemote()
	remote.failCalls[8] = true

	err = (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
	if err == nil {
		t.Fatal("configure succeeded despite injected validation failure")
	}
	if _, ok := remote.files[sourceScopedSafeSearchPath]; ok {
		t.Fatal("failed transaction left a future persistent fragment")
	}
	if _, ok := remote.files[sourceScopedSafeSearchStagedPath]; ok {
		t.Fatal("failed transaction left an inactive staged fragment")
	}
}

func TestSourceScopedSafeSearchManagerRollsBackAfterContextCancellation(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	remote := newFakeSafeSearchRemote()
	remote.failCalls[8] = true
	remote.cancelOnFailure = cancel

	err = (sourceScopedSafeSearchManager{remote: remote}).configure(ctx, source, fragment)
	if err == nil || !strings.Contains(err.Error(), "rollback completed") {
		t.Fatalf("configure error = %v, want rollback after cancellation", err)
	}
	if _, exists := remote.files[sourceScopedSafeSearchPath]; exists {
		t.Fatal("canceled transaction left persistent fragment")
	}
	if _, exists := remote.files[sourceScopedSafeSearchStagedPath]; exists {
		t.Fatal("canceled transaction left staged fragment")
	}
}

func TestSourceScopedSafeSearchManagerSurfacesEveryRollbackFailure(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	for rollbackFailCall := 9; rollbackFailCall <= 15; rollbackFailCall++ {
		t.Run(fmt.Sprintf("call-%d", rollbackFailCall), func(t *testing.T) {
			remote := newFakeSafeSearchRemote()
			remote.failCalls[8] = true
			remote.failCalls[rollbackFailCall] = true

			err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
			if err == nil || !strings.Contains(err.Error(), "rollback incomplete") {
				t.Fatalf("configure error = %v, want explicit rollback failure", err)
			}
			if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; !exists {
				t.Fatalf("incomplete rollback discarded durable recovery state: %v", remote.calls)
			}
			if rollbackFailCall == 9 {
				for _, call := range remote.calls[rollbackFailCall:] {
					if call == "reconfigure" {
						t.Fatalf("restarted after persistent restore failed: %v", remote.calls)
					}
				}
			}
		})
	}
}

func TestSourceScopedSafeSearchManagerRecoversInterruptedTransaction(t *testing.T) {
	previous := sourceScopedSafeSearchSnapshot{
		content: []byte(sourceScopedSafeSearchMarker + "\nprevious\n"),
		exists:  true,
	}
	desired := []byte(sourceScopedSafeSearchMarker + "\ndesired\n")
	remote := newFakeSafeSearchRemote()
	remote.files[sourceScopedSafeSearchBackupPath] = encodeSafeSearchBackup(previous, false, desired)
	remote.files[sourceScopedSafeSearchPath] = []byte(sourceScopedSafeSearchMarker + "\nunvalidated\n")
	remote.files[sourceScopedSafeSearchStagedPath] = []byte(sourceScopedSafeSearchMarker + "\nunvalidated\n")

	if err := (sourceScopedSafeSearchManager{remote: remote}).recoverInterruptedTransaction(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath} {
		if !bytes.Equal(remote.files[path], previous.content) {
			t.Fatalf("%s did not recover the durable previous value", path)
		}
	}
	if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; exists {
		t.Fatal("recovery retained completed rollback state")
	}
}

func TestSourceScopedSafeSearchManagerKeepsCommittedTransactionDuringRecovery(t *testing.T) {
	previous := sourceScopedSafeSearchSnapshot{
		content: []byte(sourceScopedSafeSearchMarker + "\nprevious\n"),
		exists:  true,
	}
	desired := []byte(sourceScopedSafeSearchMarker + "\ncommitted\n")
	remote := newFakeSafeSearchRemote()
	remote.files[sourceScopedSafeSearchBackupPath] = encodeSafeSearchBackup(previous, true, desired)
	remote.files[sourceScopedSafeSearchPath] = bytes.Clone(desired)
	remote.files[sourceScopedSafeSearchStagedPath] = bytes.Clone(desired)

	if err := (sourceScopedSafeSearchManager{remote: remote}).recoverInterruptedTransaction(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath} {
		if !bytes.Equal(remote.files[path], desired) {
			t.Fatalf("%s reverted an already committed transaction", path)
		}
	}
	if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; exists {
		t.Fatal("recovery retained committed rollback state")
	}
	for _, call := range remote.calls {
		if call == "reconfigure" {
			t.Fatalf("committed recovery restarted Unbound: %v", remote.calls)
		}
	}
}

func TestSourceScopedSafeSearchManagerUsesRecordedDesiredStateBeforeApplyingNextDesiredState(t *testing.T) {
	previous := sourceScopedSafeSearchSnapshot{}
	committed := []byte(sourceScopedSafeSearchMarker + "\ncommitted\n")
	next := []byte(sourceScopedSafeSearchMarker + "\nnext\n")
	remote := newFakeSafeSearchRemote()
	remote.files[sourceScopedSafeSearchBackupPath] = encodeSafeSearchBackup(previous, true, committed)
	remote.files[sourceScopedSafeSearchPath] = bytes.Clone(committed)
	remote.files[sourceScopedSafeSearchStagedPath] = bytes.Clone(committed)

	if err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), "10.100.0.0/16", next); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath} {
		if !bytes.Equal(remote.files[path], next) {
			t.Fatalf("%s did not advance to the next desired fragment", path)
		}
	}
}

func TestSourceScopedSafeSearchManagerDoesNotRestartWhenPersistentRestoreFails(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	remote := newFakeSafeSearchRemote()
	remote.failCalls[8] = true
	remote.failCalls[9] = true

	err = (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
	if err == nil || !strings.Contains(err.Error(), "restore persistent fragment without restarting") {
		t.Fatalf("configure error = %v", err)
	}
	for _, call := range remote.calls[9:] {
		if call == "reconfigure" {
			t.Fatalf("rollback restarted from unrestored persistent state: %v", remote.calls)
		}
	}
}

func TestSourceScopedSafeSearchManagerTreatsAcknowledgementLossAfterBackupDeletionAsCommitted(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	remote := newFakeSafeSearchRemote()
	remote.failAfterCalls[13] = true

	if err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment); err != nil {
		t.Fatalf("configure after acknowledged deletion loss = %v", err)
	}
	if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; exists {
		t.Fatal("post-delete acknowledgement loss recreated rollback state")
	}
	for _, path := range []string{sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath} {
		if !bytes.Equal(remote.files[path], fragment) {
			t.Fatalf("%s did not retain committed fragment", path)
		}
	}
}

func TestSourceScopedSafeSearchManagerTreatsAcknowledgementLossAfterCommitMarkerAsCommitted(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	remote := newFakeSafeSearchRemote()
	remote.failAfterCalls[12] = true

	if err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment); err != nil {
		t.Fatalf("configure after committed-marker acknowledgement loss = %v", err)
	}
	if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; exists {
		t.Fatal("committed-marker acknowledgement loss retained rollback state")
	}
	for _, path := range []string{sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath} {
		if !bytes.Equal(remote.files[path], fragment) {
			t.Fatalf("%s did not retain committed fragment", path)
		}
	}
}

func TestSourceScopedSafeSearchManagerKeepsCommittedActivationWhenBackupCleanupFails(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	remote := newFakeSafeSearchRemote()
	remote.failCalls[13] = true

	err = (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
	if err == nil || !strings.Contains(err.Error(), "activation committed") {
		t.Fatalf("configure error = %v", err)
	}
	if _, exists := remote.files[sourceScopedSafeSearchBackupPath]; !exists {
		t.Fatal("cleanup failure discarded committed recovery marker")
	}
	for _, path := range []string{sourceScopedSafeSearchPath, sourceScopedSafeSearchStagedPath} {
		if !bytes.Equal(remote.files[path], fragment) {
			t.Fatalf("%s rolled back committed fragment", path)
		}
	}
}

func TestSourceScopedSafeSearchManagerDoesNotRollbackWhenCommitOutcomeCannotBeRead(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	remote := newFakeSafeSearchRemote()
	remote.failAfterCalls[13] = true
	remote.failCalls[14] = true

	err = (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
	if err == nil || !strings.Contains(err.Error(), "commit outcome unknown") {
		t.Fatalf("configure error = %v", err)
	}
	if !bytes.Equal(remote.files[sourceScopedSafeSearchPath], fragment) {
		t.Fatal("unknown commit outcome attempted an unsafe rollback")
	}
}

func TestSafeSearchBackupValidationRejectsUnownedOrMalformedState(t *testing.T) {
	for _, content := range [][]byte{
		[]byte("garbage"),
		[]byte(sourceScopedBackupMarker + "\nunknown\nabsent\n\ninvalid\n"),
		[]byte(sourceScopedBackupMarker + "\nprepared\nabsent\npayload\ninvalid\n"),
		[]byte(sourceScopedBackupMarker + "\nprepared\npresent\n" + base64.StdEncoding.EncodeToString([]byte("# manual\n")) + "\n" + base64.StdEncoding.EncodeToString([]byte(sourceScopedSafeSearchMarker+"\ndesired\n")) + "\n"),
	} {
		if _, err := decodeSafeSearchBackup(content); err == nil {
			t.Fatalf("accepted malformed backup %q", content)
		}
	}
}

func TestUnboundConflictValidationRejectsOverlappingViews(t *testing.T) {
	for _, output := range []string{
		`/custom.conf:1: access-control-view: 10.100.1.0/24 other`,
		`/custom.conf:1: access-control-view: 10.0.0.0/8 other`,
		`/custom.conf:1: access-control-view: 10.100.1.50 other`,
		`/custom.conf:1: name: "crucible-student-safesearch"`,
		`/custom.conf:1: access-control-view: unknown other`,
	} {
		if err := validateUnboundConflictOutput(output, "10.100.0.0/16"); err == nil {
			t.Fatalf("accepted conflicting output %q", output)
		}
	}
	if err := validateUnboundConflictOutput(
		`/custom.conf:1: access-control-view: 10.101.0.0/16 other`,
		"10.100.0.0/16",
	); err != nil {
		t.Fatalf("rejected disjoint view: %v", err)
	}
	for _, output := range []string{
		`/custom.conf:1: access-control-view: fd00::/64 ipv6-view`,
		`/custom.conf:2: access-control-view: 192.168.1.50 other`,
		`/custom.conf:3: # access-control-view: 10.100.0.0/16 disabled`,
	} {
		if err := validateUnboundConflictOutput(output, "10.100.0.0/16"); err != nil {
			t.Fatalf("rejected non-conflicting view %q: %v", output, err)
		}
	}
}

func TestSourceScopedSafeSearchManagerRejectsUnownedAndConflictingFiles(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("unowned fixed path", func(t *testing.T) {
		remote := newFakeSafeSearchRemote()
		remote.files[sourceScopedSafeSearchPath] = []byte("# manual operator file\n")
		err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
		if err == nil || !strings.Contains(err.Error(), "unowned") {
			t.Fatalf("configure error = %v", err)
		}
		if len(remote.calls) != 2 {
			t.Fatalf("mutated after ownership conflict: %v", remote.calls)
		}
	})
	t.Run("other fragment conflict", func(t *testing.T) {
		remote := newFakeSafeSearchRemote()
		remote.conflict = errors.New("manual view uses source")
		err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
		if err == nil || !strings.Contains(err.Error(), "manual view uses source") {
			t.Fatalf("configure error = %v", err)
		}
		if got := strings.Join(remote.calls, ","); got != "read,read,read,conflicts" {
			t.Fatalf("mutated after external conflict: %v", remote.calls)
		}
	})
}

func TestSupportsSourceScopedSafeSearchReflectsOwnerConfiguration(t *testing.T) {
	apiOnly := New(Config{BaseURL: "https://opnsense.invalid/api"}, discardLogger())
	supported, err := apiOnly.SupportsSourceScopedSafeSearch(context.Background())
	if err != nil || supported {
		t.Fatalf("API-only SupportsSourceScopedSafeSearch = %v, %v", supported, err)
	}

	configured := New(Config{
		SSHHost:     "10.10.10.60:22",
		SSHUser:     "root",
		SSHPassword: "secret",
		SSHHostKey:  testSSHHostPublicKey(t),
	}, discardLogger())
	supported, err = configured.SupportsSourceScopedSafeSearch(context.Background())
	if err != nil || !supported {
		t.Fatalf("configured SupportsSourceScopedSafeSearch = %v, %v", supported, err)
	}

	malformed := New(Config{
		SSHHost:     "10.10.10.60:22",
		SSHUser:     "root",
		SSHPassword: "secret",
		SSHHostKey:  "not-a-key",
	}, discardLogger())
	if supported, err = malformed.SupportsSourceScopedSafeSearch(context.Background()); err == nil || supported {
		t.Fatalf("malformed SupportsSourceScopedSafeSearch = %v, %v", supported, err)
	}
}

type fakeSafeSearchRemote struct {
	files           map[string][]byte
	calls           []string
	callCount       int
	failCalls       map[int]bool
	failAfterCalls  map[int]bool
	conflict        error
	cancelOnFailure context.CancelFunc
}

func newFakeSafeSearchRemote() *fakeSafeSearchRemote {
	return &fakeSafeSearchRemote{
		files:          make(map[string][]byte),
		failCalls:      make(map[int]bool),
		failAfterCalls: make(map[int]bool),
	}
}

func (f *fakeSafeSearchRemote) record(name string) error {
	f.calls = append(f.calls, name)
	f.callCount++
	if f.failCalls[f.callCount] {
		if f.cancelOnFailure != nil {
			f.cancelOnFailure()
		}
		return fmt.Errorf("injected failure at call %d (%s)", f.callCount, name)
	}
	return nil
}

func (f *fakeSafeSearchRemote) ReadFile(ctx context.Context, path string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := f.record("read"); err != nil {
		return nil, false, err
	}
	content, exists := f.files[path]
	return bytes.Clone(content), exists, nil
}

func (f *fakeSafeSearchRemote) WriteFileAtomic(ctx context.Context, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.record("write"); err != nil {
		return err
	}
	f.files[path] = bytes.Clone(content)
	if f.failAfterCalls[f.callCount] {
		return fmt.Errorf("injected post-effect failure at call %d (write)", f.callCount)
	}
	return nil
}

func (f *fakeSafeSearchRemote) CopyFileAtomic(ctx context.Context, source, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.record("copy"); err != nil {
		return err
	}
	f.files[destination] = bytes.Clone(f.files[source])
	return nil
}

func (f *fakeSafeSearchRemote) RemoveFile(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.record("remove"); err != nil {
		return err
	}
	delete(f.files, path)
	if f.failAfterCalls[f.callCount] {
		return fmt.Errorf("injected post-effect failure at call %d (remove)", f.callCount)
	}
	return nil
}

func (f *fakeSafeSearchRemote) CheckConflicts(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.record("conflicts"); err != nil {
		return err
	}
	return f.conflict
}

func (f *fakeSafeSearchRemote) CheckUnbound(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.record("check")
}

func (f *fakeSafeSearchRemote) ReconfigureUnbound(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.record("reconfigure"); err != nil {
		return err
	}
	content, exists := f.files[sourceScopedSafeSearchPath]
	if exists {
		f.files[sourceScopedSafeSearchStagedPath] = bytes.Clone(content)
	} else {
		delete(f.files, sourceScopedSafeSearchStagedPath)
	}
	return nil
}
