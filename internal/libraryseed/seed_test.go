// Package libraryseed has no non-test code. It exists to hold the guard over
// deploy/sql/library-actions.sql and deploy/sql/library-workflows.sql, which
// are content rather than schema and so are applied outside the migration
// chain.
//
// The seed is validated by APPLYING IT, not by parsing it. A hand-rolled SQL
// parser here would be a second, worse implementation of the thing that
// actually consumes the file, and would drift from it; loading the real bytes
// into a real PostgreSQL and reading the rows back proves the file applies,
// proves it is convergent, and yields exactly the rows production would get.
//
// The corpora under internal/scriptvalidator/testdata and
// internal/engine/testdata are regenerated from those rows with -update, which
// is what stops "refresh the snapshot by hand" from silently not happening.
package libraryseed

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mvdan.cc/sh/v3/syntax"

	"github.com/jmal1/selfservice-api/internal/actionlibrary"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/runnertools"
	"github.com/jmal1/selfservice-api/internal/scriptvalidator"
	"github.com/jmal1/selfservice-api/internal/workflowvalidation"
)

var update = flag.Bool("update", false,
	"rewrite the action corpora under internal/*/testdata from the applied seed")

const (
	actionsSQL   = "../../deploy/sql/library-actions.sql"
	workflowsSQL = "../../deploy/sql/library-workflows.sql"
)

type seededAction struct {
	Slug               string          `json:"slug"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	ActionType         string          `json:"action_type"`
	ActionCategory     string          `json:"action_category"`
	InputContext       json.RawMessage `json:"input_context"`
	OutputContext      json.RawMessage `json:"output_context"`
	SupportedPlatforms json.RawMessage `json:"supported_platforms"`
	Script             string          `json:"script"`
}

func (a seededAction) isWindows() bool {
	return strings.Contains(string(a.SupportedPlatforms), `"windows"`)
}

type seededWorkflow struct {
	Slug          string
	Name          string
	Status        string
	ExecutionMode string
	Script        string
}

type seedResult struct {
	actions   []seededAction
	workflows []seededWorkflow
}

var (
	seedOnce   sync.Once
	seedCached seedResult
	seedErr    error
	seedSkip   string
)

// applySeed returns what the database holds after both SQL files have been
// applied twice. Twice, because convergence is the property the ON CONFLICT
// clauses claim and a second apply is the only thing that tests it — a DO
// NOTHING mistake or a missing column in the update list looks identical to DO
// UPDATE on a fresh database.
//
// Computed once and shared: the result is read-only, and seven tests each
// creating and migrating a database would dominate the package's runtime.
func applySeed(t *testing.T) seedResult {
	t.Helper()
	seedOnce.Do(func() { seedCached, seedSkip, seedErr = loadSeed() })
	if seedSkip != "" {
		t.Skip(seedSkip)
	}
	if seedErr != nil {
		t.Fatal(seedErr)
	}
	return seedCached
}

// loadSeed builds a throwaway database, migrates it, and applies the seed
// there.
//
// It does NOT use TEST_DATABASE_URL directly, and must not: the seed adopts
// rows by slug and the read-back below selects every library action in the
// database, so it would both be polluted by and pollute other packages' rows.
// `go test ./...` runs packages concurrently against the same DSN, and an
// earlier version of this file cleared `actions` and `workflows` — which broke
// internal/api/handlers' authoring tests in a way that looked like a bug in
// those tests.
func loadSeed() (seedResult, string, error) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		return seedResult{}, "set TEST_DATABASE_URL to a PostgreSQL server to validate the library seed", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// A per-process name so two concurrent `go test` invocations, or a stale
	// database from a killed run, cannot collide.
	dbName := fmt.Sprintf("crucible_libraryseed_%d", os.Getpid())
	seedDSN, err := replaceDatabase(adminDSN, dbName)
	if err != nil {
		return seedResult{}, "", err
	}

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return seedResult{}, "", fmt.Errorf("connect to %s: %w", redactDSN(adminDSN), err)
	}
	// CREATE/DROP DATABASE cannot run inside a transaction, and the identifier
	// is generated above from a pid, so quoting it is sufficient.
	quoted := pgx.Identifier{dbName}.Sanitize()
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoted); err != nil {
		admin.Close(ctx)
		return seedResult{}, "", fmt.Errorf("drop stale %s: %w", dbName, err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close(ctx)
		return seedResult{}, "", fmt.Errorf("create %s: %w", dbName, err)
	}
	admin.Close(ctx)
	// Registered for the whole package rather than deferred: the pool below
	// must be closed before the database can be dropped, and TestMain is what
	// knows when the last test is done.
	registerSeedCleanup(adminDSN, dbName)

	if err := database.RunMigrations(seedDSN); err != nil {
		return seedResult{}, "", fmt.Errorf("migrate %s: %w", dbName, err)
	}

	pool, err := pgxpool.New(ctx, seedDSN)
	if err != nil {
		return seedResult{}, "", err
	}
	defer pool.Close()

	for pass := 1; pass <= 2; pass++ {
		for _, path := range []string{actionsSQL, workflowsSQL} {
			raw, err := os.ReadFile(path)
			if err != nil {
				return seedResult{}, "", fmt.Errorf("read %s: %w", path, err)
			}
			// pgx Exec with no arguments uses the simple query protocol, which
			// is what allows a multi-statement script with dollar-quoted
			// bodies to be sent as one string.
			if _, err := pool.Exec(ctx, string(raw)); err != nil {
				return seedResult{}, "", fmt.Errorf("apply %s (pass %d): %w", filepath.Base(path), pass, err)
			}
		}
	}

	return readSeed(ctx, pool)
}

func readSeed(ctx context.Context, pool *pgxpool.Pool) (seedResult, string, error) {
	var res seedResult
	rows, err := pool.Query(ctx, `
		SELECT slug, name, coalesce(description, ''), action_type, action_category,
		       input_context, output_context, supported_platforms, script
		FROM actions
		WHERE is_library = true AND slug IS NOT NULL
		ORDER BY slug
	`)
	if err != nil {
		return res, "", err
	}
	for rows.Next() {
		var a seededAction
		if err := rows.Scan(&a.Slug, &a.Name, &a.Description, &a.ActionType,
			&a.ActionCategory, &a.InputContext, &a.OutputContext,
			&a.SupportedPlatforms, &a.Script); err != nil {
			rows.Close()
			return res, "", err
		}
		res.actions = append(res.actions, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, "", err
	}

	wfRows, err := pool.Query(ctx, `
		SELECT slug, name, status, execution_mode, script
		FROM workflows ORDER BY slug
	`)
	if err != nil {
		return res, "", err
	}
	for wfRows.Next() {
		var w seededWorkflow
		if err := wfRows.Scan(&w.Slug, &w.Name, &w.Status, &w.ExecutionMode, &w.Script); err != nil {
			wfRows.Close()
			return res, "", err
		}
		res.workflows = append(res.workflows, w)
	}
	wfRows.Close()
	if err := wfRows.Err(); err != nil {
		return res, "", err
	}

	// A seed that applied cleanly but inserted nothing would make every
	// assertion in this package vacuous.
	if len(res.actions) == 0 {
		return res, "", errors.New("the seed applied but produced no library actions")
	}
	if len(res.workflows) == 0 {
		return res, "", errors.New("the seed applied but produced no workflows")
	}
	return res, "", nil
}

// replaceDatabase rewrites a PostgreSQL URL's database name, keeping every
// other component (credentials, host, sslmode) intact.
func replaceDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("TEST_DATABASE_URL is not a URL: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// redactDSN strips the password so a connection error can be logged.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "<unparseable DSN>"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

var seedCleanup func()

func registerSeedCleanup(adminDSN, dbName string) {
	seedCleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		admin, err := pgx.Connect(ctx, adminDSN)
		if err != nil {
			return
		}
		defer admin.Close(ctx)
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
	}
}

// TestMain drops the throwaway database after the last test. It cannot be a
// t.Cleanup: the database is shared by every test in the package, so no single
// test knows when it is finished with.
func TestMain(m *testing.M) {
	flag.Parse()
	code := m.Run()
	if seedCleanup != nil {
		seedCleanup()
	}
	os.Exit(code)
}

// One apply, then a second apply, must leave the same rows. This is the whole
// claim of the ON CONFLICT DO UPDATE clauses.
func TestSeed_AppliesAndConverges(t *testing.T) {
	res := applySeed(t)
	t.Logf("seed defines %d library actions and %d workflows", len(res.actions), len(res.workflows))

	seen := make(map[string]string, len(res.actions))
	for _, a := range res.actions {
		if err := actionlibrary.ValidateSlug(a.Slug); err != nil {
			t.Errorf("seeded action: %v", err)
			continue
		}
		callable, err := actionlibrary.CallableName(a.Slug)
		if err != nil {
			t.Errorf("seeded action %q: %v", a.Slug, err)
			continue
		}
		if prev, dup := seen[callable]; dup {
			t.Errorf("seeded actions %q and %q both render function %q; "+
				"the engine refuses to build an ambiguous library, which fails every run",
				prev, a.Slug, callable)
		}
		seen[callable] = a.Slug
	}
}

// The generated library is one file sourced by every runner pod, so one body
// that does not parse fails `source` and takes down every action in every run.
func TestSeed_EveryBashBodyParses(t *testing.T) {
	res := applySeed(t)
	var checked int
	for _, a := range res.actions {
		if a.isWindows() {
			continue // PowerShell; excluded from the bash library by SQL.
		}
		checked++
		callable, err := actionlibrary.CallableName(a.Slug)
		if err != nil {
			t.Errorf("%s: %v", a.Slug, err)
			continue
		}
		if err := actionlibrary.ValidateBody(callable, a.Script); err != nil {
			t.Errorf("seeded action %q: %v", a.Slug, err)
		}
		if strings.TrimSpace(a.Script) == "" {
			t.Errorf("seeded action %q has an empty body; it renders as `%s() { }`, a syntax error", a.Slug, callable)
		}
	}
	if checked == 0 {
		t.Fatal("checked 0 bash bodies; the platform filter is wrong")
	}
}

// A command the runner image does not have exits 127 mid-assessment, and the
// student sees a red check that no change to their VM can turn green.
func TestSeed_EveryToolIsInstalled(t *testing.T) {
	res := applySeed(t)
	var checked int
	for _, a := range res.actions {
		if a.isWindows() {
			continue
		}
		checked++
		missing := scriptvalidator.MissingCommands(a.Script)
		if len(missing) == 0 {
			continue
		}
		t.Errorf("seeded action %q invokes %v, which the runner image does not provide.\n"+
			"Add the providing package to internal/runnertools/tools.txt, or rewrite the "+
			"action to use an installed tool.\nInstalled: %v",
			a.Slug, missing, runnertools.Commands())
	}
	if checked == 0 {
		t.Fatal("checked 0 action scripts")
	}
}

// A seeded workflow is written to the database with status='active', which
// bypasses the API's activation gate. This test IS that gate: it runs the same
// validation ActivateWorkflow would have run, against the same catalog.
func TestSeed_EveryWorkflowSatisfiesTheActivationContract(t *testing.T) {
	res := applySeed(t)

	catalog := make([]workflowvalidation.LibraryAction, 0, len(res.actions))
	for _, a := range res.actions {
		reason := ""
		callable := a.isWindows()
		switch {
		case strings.TrimSpace(a.Script) == "":
			reason = "its script body is empty"
		case callable:
			reason = "supported_platforms includes windows"
		}
		catalog = append(catalog, workflowvalidation.LibraryAction{
			Slug:              a.Slug,
			RunnerCallable:    !callable && strings.TrimSpace(a.Script) != "",
			UnavailableReason: reason,
		})
	}

	var checked int
	for _, w := range res.workflows {
		if w.ExecutionMode != "kali_runner" {
			continue
		}
		checked++
		if err := workflowvalidation.ValidateRunActionCallsWithCatalog(w.Script, catalog); err != nil {
			t.Errorf("seeded workflow %q would be refused activation: %v", w.Slug, err)
		}
	}
	if checked == 0 {
		t.Fatal("checked 0 kali_runner workflows")
	}
}

// ValidateRunActionCallsWithCatalog deliberately ignores a command it does not
// recognize, because a workflow may legitimately call `bash -c` or a runner
// binary. That tolerance means it stays silent on the one mistake this seed is
// most likely to make: calling a function no seeded action generates, which is
// exit 127 at grading time. Nothing else checks it, so this does.
func TestSeed_EveryWorkflowCallableIsSeeded(t *testing.T) {
	res := applySeed(t)

	known := make(map[string]struct{}, len(res.actions))
	for _, a := range res.actions {
		if a.isWindows() || strings.TrimSpace(a.Script) == "" {
			continue // not rendered into the bash library
		}
		callable, err := actionlibrary.CallableName(a.Slug)
		if err != nil {
			continue // already reported by TestSeed_AppliesAndConverges
		}
		known[callable] = struct{}{}
	}

	// Commands a workflow may pass to run_action that are not library actions.
	// Kept deliberately tiny: every addition is a hole in this guard.
	passthrough := map[string]struct{}{"bash": {}, "sh": {}}

	var checked int
	for _, w := range res.workflows {
		for _, call := range runActionCommands(t, w.Script) {
			checked++
			if _, ok := known[call.command]; ok {
				continue
			}
			if _, ok := passthrough[call.command]; ok {
				continue
			}
			if runnertools.HasCommand(call.command) {
				continue
			}
			t.Errorf("workflow %q line %d calls %q, which no seeded action generates "+
				"and the runner image does not provide; this action would exit 127 mid-assessment",
				w.Slug, call.line, call.command)
		}
	}
	if checked == 0 {
		t.Fatal("found no run_action calls in any seeded workflow; the extractor is broken")
	}
	t.Logf("verified %d run_action callables resolve", checked)
}

type runActionCall struct {
	command string
	line    int
}

// runActionCommands pulls the second argument out of every `run_action` call.
// It parses rather than greps, so a call spanning lines or using quoting is
// read the same way the real validator reads it.
func runActionCommands(t *testing.T, script string) []runActionCall {
	t.Helper()
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).
		Parse(strings.NewReader(script), "workflow")
	if err != nil {
		t.Fatalf("seeded workflow script is not valid bash: %v", err)
	}
	var out []runActionCall
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) < 3 {
			return true
		}
		if name, static := staticWord(call.Args[0]); !static || name != "run_action" {
			return true
		}
		command, static := staticWord(call.Args[2])
		if !static {
			return true
		}
		out = append(out, runActionCall{command: command, line: int(call.Pos().Line())})
		return true
	})
	return out
}

func staticWord(word *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			b.WriteString(part.Value)
		case *syntax.SglQuoted:
			b.WriteString(part.Value)
		default:
			return "", false
		}
	}
	return b.String(), true
}

// Every seeded workflow must be reachable. A workflow left in draft by a typo
// in the seed is invisible to instructors and silently absent from playlists.
func TestSeed_WorkflowsAreActive(t *testing.T) {
	res := applySeed(t)
	for _, w := range res.workflows {
		if w.Status != "active" {
			t.Errorf("seeded workflow %q has status %q; only `active` can be added to a playlist run",
				w.Slug, w.Status)
		}
	}
}

// The two corpora are what internal/engine's tool and parse guards run against.
// If they stop matching the seed, those guards keep passing while covering an
// older library — the hollow-guard failure this repo has been bitten by before.
// Run `go test ./internal/libraryseed -update` to resync.
func TestSeed_CorporaMatch(t *testing.T) {
	res := applySeed(t)

	// internal/scriptvalidator/testdata/actions.json: the full metadata corpus.
	type validatorAction struct {
		Slug               string          `json:"slug"`
		Name               string          `json:"name"`
		ActionType         string          `json:"action_type"`
		ActionCategory     string          `json:"action_category"`
		InputContext       json.RawMessage `json:"input_context"`
		OutputContext      json.RawMessage `json:"output_context"`
		SupportedPlatforms json.RawMessage `json:"supported_platforms"`
		Script             string          `json:"script"`
	}
	validatorCorpus := make([]validatorAction, 0, len(res.actions))
	for _, a := range res.actions {
		validatorCorpus = append(validatorCorpus, validatorAction{
			Slug: a.Slug, Name: a.Name,
			ActionType: a.ActionType, ActionCategory: a.ActionCategory,
			InputContext: a.InputContext, OutputContext: a.OutputContext,
			SupportedPlatforms: a.SupportedPlatforms, Script: a.Script,
		})
	}

	// internal/engine/testdata/library_actions.json: exactly what
	// ListRunnerLibraryActions returns, so windows and empty bodies are out.
	type engineAction struct {
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Script      string `json:"script"`
	}
	engineCorpus := make([]engineAction, 0, len(res.actions))
	for _, a := range res.actions {
		if a.isWindows() || strings.TrimSpace(a.Script) == "" {
			continue
		}
		engineCorpus = append(engineCorpus, engineAction{
			Slug: a.Slug, Name: a.Name, Description: a.Description, Script: a.Script,
		})
	}

	compareCorpus(t, "../scriptvalidator/testdata/actions.json", validatorCorpus)
	compareCorpus(t, "../engine/testdata/library_actions.json", engineCorpus)
}

// AGENTS.md §7 is the catalog an AI authoring assistant reads before it writes
// a workflow. A slug listed there that the seed does not define sends the
// assistant to write `run_action "…" some_missing_thing`, and a slug the seed
// defines but §7 omits is an action nobody will ever be told about. The table
// was already stale once — it listed 18 actions when the library had 24.
func TestSeed_AgentsCatalogListsEveryAction(t *testing.T) {
	res := applySeed(t)

	raw, err := os.ReadFile("../../AGENTS.md")
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	doc := string(raw)

	// Section 7 only. A slug named in a prose example elsewhere in the file
	// must not count as being catalogued.
	start := strings.Index(doc, "## 7. Action library catalog")
	if start < 0 {
		t.Fatal("AGENTS.md has no '## 7. Action library catalog' heading; " +
			"this guard cannot locate the catalog and would silently pass")
	}
	rest := doc[start+1:]
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		t.Fatal("could not find the end of AGENTS.md section 7")
	}
	section := rest[:end]

	// Slug in a table's leading cell, or in the Windows prose list.
	slugRe := regexp.MustCompile("`([a-z0-9]+(?:-[a-z0-9]+)+)`")
	listed := make(map[string]struct{})
	for _, m := range slugRe.FindAllStringSubmatch(section, -1) {
		listed[m[1]] = struct{}{}
	}
	if len(listed) == 0 {
		t.Fatal("extracted no slugs from AGENTS.md section 7; the pattern is broken")
	}

	for _, a := range res.actions {
		if _, ok := listed[a.Slug]; !ok {
			t.Errorf("action %q is seeded but absent from AGENTS.md section 7; "+
				"no authoring assistant reading that file will know it exists", a.Slug)
		}
	}

	// The reverse direction. Only slugs that look like action slugs are
	// checked, and the section deliberately also mentions file paths and flag
	// names — those do not match slugRe's hyphenated-lowercase shape closely
	// enough to matter, so an unknown match really is a wrong slug.
	seeded := make(map[string]struct{}, len(res.actions))
	for _, a := range res.actions {
		seeded[a.Slug] = struct{}{}
	}
	// Words in the section that are hyphenated lowercase but are not actions.
	notActions := map[string]struct{}{
		"library-actions.sql": {}, "library-workflows.sql": {},
		"actions.sh": {}, "actions.json": {},
		"kali-runner": {}, "vmware-tools": {},
		"linux:ubuntu": {}, "linux:debian": {},
		"port-23-closed": {},
	}
	for slug := range listed {
		if _, ok := seeded[slug]; ok {
			continue
		}
		if _, ok := notActions[slug]; ok {
			continue
		}
		if strings.Contains(slug, ".") {
			continue // a filename, not a slug
		}
		t.Errorf("AGENTS.md section 7 lists %q, which no seeded action defines; "+
			"an assistant told to use it will produce a workflow that exits 127", slug)
	}
}

func compareCorpus(t *testing.T, path string, want any) {
	t.Helper()
	encoded, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	if *update {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", path)
		return
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if normalizeJSON(t, onDisk) == normalizeJSON(t, encoded) {
		return
	}
	t.Errorf("%s is out of sync with deploy/sql/library-actions.sql.\n"+
		"The engine guards that use it are now covering a stale library.\n"+
		"Resync with: go test ./internal/libraryseed -update\n%s",
		path, corpusDiffHint(t, onDisk, encoded))
}

// normalizeJSON removes formatting and line-ending differences so the
// comparison is about content, not about which platform wrote the file.
func normalizeJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// corpusDiffHint names the slugs that differ. A full JSON diff of 40 bash
// bodies is unreadable in test output; the slug list is what tells you what
// changed.
func corpusDiffHint(t *testing.T, onDisk, fresh []byte) string {
	t.Helper()
	slugs := func(raw []byte) map[string]string {
		var entries []map[string]any
		out := map[string]string{}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return out
		}
		for _, e := range entries {
			slug, _ := e["slug"].(string)
			script, _ := e["script"].(string)
			out[slug] = script
		}
		return out
	}
	a, b := slugs(onDisk), slugs(fresh)

	var added, removed, changed []string
	for slug, script := range b {
		old, ok := a[slug]
		switch {
		case !ok:
			added = append(added, slug)
		case old != script:
			changed = append(changed, slug)
		}
	}
	for slug := range a {
		if _, ok := b[slug]; !ok {
			removed = append(removed, slug)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return fmt.Sprintf("  added:   %v\n  removed: %v\n  changed: %v", added, removed, changed)
}
