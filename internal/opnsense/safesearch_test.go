package opnsense

import (
	"bytes"
	"context"
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
	if got := strings.Join(remote.calls, ","); got != "read,read,conflicts,write,copy,check,reconfigure,read,read" {
		t.Fatalf("transaction calls = %s", got)
	}
}

func TestSourceScopedSafeSearchManagerRollsBackEveryForwardBoundary(t *testing.T) {
	previous := []byte(sourceScopedSafeSearchMarker + "\nprevious persistent\n")
	previousStaged := []byte(sourceScopedSafeSearchMarker + "\nprevious staged\n")
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}

	for failCall := 4; failCall <= 9; failCall++ {
		t.Run(fmt.Sprintf("call-%d", failCall), func(t *testing.T) {
			remote := newFakeSafeSearchRemote()
			remote.files[sourceScopedSafeSearchPath] = bytes.Clone(previous)
			remote.files[sourceScopedSafeSearchStagedPath] = bytes.Clone(previousStaged)
			remote.failCalls[failCall] = true

			err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
			if err == nil || !strings.Contains(err.Error(), "rollback completed") {
				t.Fatalf("configure error = %v, want completed rollback", err)
			}
			if !bytes.Equal(remote.files[sourceScopedSafeSearchPath], previous) {
				t.Errorf("persistent fragment not restored after call %d", failCall)
			}
			if !bytes.Equal(remote.files[sourceScopedSafeSearchStagedPath], previousStaged) {
				t.Errorf("staged fragment not restored after call %d", failCall)
			}
			if !endsWith(remote.calls, "check", "reconfigure") {
				t.Fatalf("rollback did not validate and reconfigure after call %d: %v", failCall, remote.calls)
			}
		})
	}
}

func TestSourceScopedSafeSearchManagerPreflightFailuresDoNotMutate(t *testing.T) {
	fragment, source, err := renderSourceScopedSafeSearch("10.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	for failCall := 1; failCall <= 3; failCall++ {
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
			for _, call := range remote.calls {
				if call == "write" || call == "copy" || call == "remove" || call == "reconfigure" {
					t.Fatalf("preflight failure mutated configuration: %v", remote.calls)
				}
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
	remote.failCalls[6] = true

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
	remote.failCalls[6] = true
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
	for rollbackFailCall := 7; rollbackFailCall <= 10; rollbackFailCall++ {
		t.Run(fmt.Sprintf("call-%d", rollbackFailCall), func(t *testing.T) {
			remote := newFakeSafeSearchRemote()
			remote.failCalls[6] = true
			remote.failCalls[rollbackFailCall] = true

			err := (sourceScopedSafeSearchManager{remote: remote}).configure(context.Background(), source, fragment)
			if err == nil || !strings.Contains(err.Error(), "rollback incomplete") {
				t.Fatalf("configure error = %v, want explicit rollback failure", err)
			}
			if len(remote.calls) < 10 || remote.calls[len(remote.calls)-1] != "reconfigure" {
				t.Fatalf("rollback stopped silently at call %d: %v", rollbackFailCall, remote.calls)
			}
		})
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
		if len(remote.calls) != 1 {
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
		if got := strings.Join(remote.calls, ","); got != "read,read,conflicts" {
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
	conflict        error
	cancelOnFailure context.CancelFunc
}

func newFakeSafeSearchRemote() *fakeSafeSearchRemote {
	return &fakeSafeSearchRemote{
		files:     make(map[string][]byte),
		failCalls: make(map[int]bool),
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
	f.files[path] = bytes.Clone(content)
	return f.record("write")
}

func (f *fakeSafeSearchRemote) CopyFileAtomic(ctx context.Context, source, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.files[destination] = bytes.Clone(f.files[source])
	return f.record("copy")
}

func (f *fakeSafeSearchRemote) RemoveFile(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(f.files, path)
	return f.record("remove")
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
	return f.record("reconfigure")
}

func endsWith(values []string, suffix ...string) bool {
	if len(values) < len(suffix) {
		return false
	}
	return strings.Join(values[len(values)-len(suffix):], "\x00") == strings.Join(suffix, "\x00")
}
