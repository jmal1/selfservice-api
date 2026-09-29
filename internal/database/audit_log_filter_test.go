package database

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAuditLogWhere_PeopleHideMonitorsAndRawHTTP(t *testing.T) {
	where, args, err := auditLogWhere(AuditLogFilter{
		Actor:          "human",
		ExcludeActions: []string{"api.request"},
		Actions:        []string{"pod.", "vm."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(where, "a.user_id IS NOT NULL") || !strings.Contains(where, "NOT LIKE 'synthetic%'") {
		t.Fatalf("human filter missing: %s", where)
	}
	if !strings.Contains(where, "a.action <> $3") {
		t.Fatalf("exclude placeholder: %s", where)
	}
	if !strings.Contains(where, "LIKE $1 ESCAPE '\\' OR a.action LIKE $2 ESCAPE '\\'") {
		t.Fatalf("action prefixes: %s", where)
	}
	if len(args) != 3 || args[0] != "pod.%" || args[1] != "vm.%" || args[2] != "api.request" {
		t.Fatalf("args: %#v", args)
	}
}

func TestAuditLogWhere_SyntheticAndSearchEscape(t *testing.T) {
	where, args, err := auditLogWhere(AuditLogFilter{
		Actor: "synthetic",
		Query: `100%_lab`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(where, "u.username LIKE 'synthetic%'") {
		t.Fatalf("synthetic filter missing: %s", where)
	}
	if !strings.Contains(where, "u.display_name") || !strings.Contains(where, "a.details::text") {
		t.Fatalf("search columns missing: %s", where)
	}
	if len(args) != 1 || args[0] != `%100\%\_lab%` {
		t.Fatalf("escaped search: %#v", args)
	}
}

func TestAuditLogWhere_SystemAndLegacyPrefix(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	where, args, err := auditLogWhere(AuditLogFilter{
		Actor:        "system",
		Action:       "auth.",
		UserID:       &id,
		ResourceType: "pod",
		Since:        &since,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(where, "a.user_id IS NULL") {
		t.Fatalf("system filter missing: %s", where)
	}
	if args[0] != `auth.%` {
		t.Fatalf("legacy prefix: %#v", args[0])
	}
	if !strings.Contains(where, "a.user_id = $2") || !strings.Contains(where, "a.resource_type = $3") || !strings.Contains(where, "a.created_at >= $4") {
		t.Fatalf("remaining filters: %s", where)
	}
}

func TestAuditLogWhere_RejectsBadActorAndSize(t *testing.T) {
	if _, _, err := auditLogWhere(AuditLogFilter{Actor: "robot"}); err == nil {
		t.Fatal("expected actor error")
	}
	actions := make([]string, 9)
	for i := range actions {
		actions[i] = "pod."
	}
	if _, _, err := auditLogWhere(AuditLogFilter{Actions: actions}); err == nil {
		t.Fatal("expected size error")
	}
	if _, _, err := auditLogWhere(AuditLogFilter{Query: strings.Repeat("a", 201)}); err == nil {
		t.Fatal("expected search length error")
	}
}

func TestAuditLogWhere_UnderscoreIsLiteral(t *testing.T) {
	_, args, err := auditLogWhere(AuditLogFilter{Action: "pod_"})
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != `pod\_%` {
		t.Fatalf("prefix escape: %#v", args[0])
	}
}
