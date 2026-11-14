package steps

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestAccessDefaultPrivilegesSQL(t *testing.T) {
	got := AccessDefaultPrivilegesSQL(`a."b`)
	want := []string{
		`ALTER DEFAULT PRIVILEGES FOR ROLE "a.""b_owner" GRANT USAGE ON SCHEMAS TO "a.""b_app", "a.""b_ro";`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "a.""b_owner" GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO "a.""b_app";`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "a.""b_owner" GRANT USAGE, SELECT ON SEQUENCES TO "a.""b_app";`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "a.""b_owner" GRANT EXECUTE ON FUNCTIONS TO "a.""b_app";`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "a.""b_owner" GRANT SELECT ON TABLES TO "a.""b_ro";`,
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d statements, want %d: %s", len(lines), len(want), got)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("statement %d: got %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestAccessReconcileSQL(t *testing.T) {
	sql := AccessReconcileSQL(`a."$vaire$b`)
	for _, want := range []string{
		"BEGIN;\nSET LOCAL search_path = pg_catalog;",
		`DO $vaire_$`,
		`END;` + "\n" + `$vaire_$;`,
		`COMMIT;`,
		`nspowner = E'"a.""$vaire$b_owner"'::regrole`,
		`relowner = E'"a.""$vaire$b_owner"'::regrole`,
		`proowner = E'"a.""$vaire$b_owner"'::regrole`,
		`d.objsubid = 0`,
		`d.refclassid = 'pg_extension'::regclass`,
		`d.refobjsubid = 0 AND d.deptype = 'e'`,
		`GRANT USAGE ON SCHEMA %I TO %I, %I`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE %I.%I TO %I`,
		`GRANT SELECT ON TABLE %I.%I TO %I`,
		`GRANT USAGE, SELECT ON SEQUENCE %I.%I TO %I`,
		`GRANT EXECUTE ON ROUTINE %I.%I(%s) TO %I`,
		`pg_get_function_identity_arguments(p.oid)`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, forbidden := range []string{"REVOKE", "GRANT CREATE", "GRANT ALL", "ON TYPES"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("unexpected %q", forbidden)
		}
	}
	if !strings.HasSuffix(sql, "COMMIT;\n") {
		t.Fatal("transaction does not end with commit")
	}
}

func TestAccessInspectionSQL(t *testing.T) {
	sql := AccessInspectionSQL(`a."b`)
	for _, want := range []string{
		`d.defaclnamespace = 0`,
		`aclexplode(d.defaclacl)`,
		`a.grantee = r.grantee`,
		`nspowner = E'"a.""b_owner"'::regrole`,
		`d.objsubid = 0`,
		`d.refclassid = 'pg_extension'::regclass`,
		`d.refobjsubid = 0 AND d.deptype = 'e'`,
		`has_schema_privilege`,
		`has_table_privilege`,
		`has_sequence_privilege`,
		`has_function_privilege`,
		`uncovered_creators`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(sql, "acldefault(") {
		t.Error("object ACLs must not be substituted with creator defaults")
	}
}

func TestInspectAccess(t *testing.T) {
	good := `{"identities":[1,2,3],"default_missing":0,"schema_missing":0,"table_missing":0,"sequence_missing":0,"routine_missing":0,"uncovered_creators":["other"]}`
	tests := []struct {
		name, raw, status string
		err               error
		wantErr           bool
	}{
		{"installed", good, "installed", nil, false},
		{"default drift", strings.Replace(good, `"default_missing":0`, `"default_missing":1`, 1), "pending", nil, false},
		{"schema drift", strings.Replace(good, `"schema_missing":0`, `"schema_missing":1`, 1), "pending", nil, false},
		{"table drift", strings.Replace(good, `"table_missing":0`, `"table_missing":1`, 1), "pending", nil, false},
		{"sequence drift", strings.Replace(good, `"sequence_missing":0`, `"sequence_missing":1`, 1), "pending", nil, false},
		{"routine drift", strings.Replace(good, `"routine_missing":0`, `"routine_missing":1`, 1), "pending", nil, false},
		{"query failed", "", "unknown", errors.New("offline"), true},
		{"malformed", "{", "unknown", nil, true},
		{"missing", `{}`, "unknown", nil, true},
		{"missing role", strings.Replace(good, `[1,2,3]`, `[1,0,3]`, 1), "unknown", nil, true},
		{"negative", strings.Replace(good, `"table_missing":0`, `"table_missing":-1`, 1), "unknown", nil, true},
		{"null creators", strings.Replace(good, `["other"]`, `null`, 1), "unknown", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InspectAccess(context.Background(), "db", func(_ context.Context, sql string) (string, error) {
				if sql != AccessInspectionSQL("db") {
					t.Error("wrong inspection query")
				}
				return tt.raw, tt.err
			})
			if (err != nil) != tt.wantErr || got.Status != tt.status {
				t.Fatalf("got %+v, %v", got, err)
			}
			if !tt.wantErr && (len(got.UncoveredCreators) != 1 || got.UncoveredCreators[0] != "other") {
				t.Fatalf("creators lost: %+v", got)
			}
		})
	}
	got, err := InspectAccess(context.Background(), "db", nil)
	if err == nil || got.Status != "unknown" {
		t.Fatalf("missing adapter: %+v, %v", got, err)
	}
}

func TestReconcileAccessFailurePropagates(t *testing.T) {
	fail := errors.New("SQL failed, transaction rolled back on session close")
	calls := 0
	err := ReconcileAccess(context.Background(), "db", func(_ context.Context, sql string) error {
		calls++
		if !strings.HasPrefix(sql, "BEGIN;\n") || !strings.HasSuffix(sql, "COMMIT;\n") {
			t.Fatal("defaults and grants must be one transaction")
		}
		if strings.Index(sql, "ALTER DEFAULT PRIVILEGES") > strings.Index(sql, "DO $") {
			t.Fatal("defaults must precede reconciliation")
		}
		return fail
	})
	if calls != 1 || !errors.Is(err, fail) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if ReconcileAccess(context.Background(), "db", nil) == nil {
		t.Fatal("missing executor accepted")
	}
}

func TestPostgresAccessStepDriftAndJournal(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}}}
	path := filepath.Join(t.TempDir(), "journal.json")
	j := &engine.Journal{Steps: map[string]engine.JournalEntry{
		"postgres-roles-v1@app": {Status: engine.StatusCompleted},
	}}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	status := "pending"
	calls := 0
	step := postgresAccess{
		inspect: func(context.Context, config.ClusterSpec) (AccessCoverage, error) {
			return AccessCoverage{Status: status, DefaultMissing: 1}, nil
		},
		reconcile: func(context.Context, config.ClusterSpec) error {
			calls++
			status = "installed"
			return nil
		},
	}
	plan, err := engine.Plan(context.Background(), []engine.Step{step}, cfg, engine.Runtime{}, engine.Options{JournalPath: path})
	if err != nil || plan.WouldApply != 1 || len(plan.AccessConfirmTokens) != 1 || plan.AccessConfirmTokens[0] != "app/access" {
		t.Fatalf("old role journal hid access: %+v, %v", plan, err)
	}
	_, _, err = engine.Apply(context.Background(), []engine.Step{step}, cfg, engine.Runtime{}, engine.Options{
		JournalPath: path, AccessConfirm: map[string]bool{"app": true},
	})
	if err != nil || calls != 1 {
		t.Fatalf("authorized apply: calls %d, %v", calls, err)
	}
	status = "pending"
	_, _, err = engine.Apply(context.Background(), []engine.Step{step}, cfg, engine.Runtime{}, engine.Options{JournalPath: path})
	if err == nil || calls != 1 {
		t.Fatalf("drift bypassed confirmation: calls %d, %v", calls, err)
	}
	status = "unknown"
	plan, err = engine.Plan(context.Background(), []engine.Step{step}, cfg, engine.Runtime{}, engine.Options{JournalPath: path})
	if err == nil {
		t.Fatalf("unknown inspection accepted: %+v", plan)
	}
}

func TestPostgresAccessInputsIgnoreProtectionAndFollowerSkips(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}}}
	step := postgresAccess{}.Instance("app")
	before, err := step.Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Clusters[0].Protected = true
	after, err := step.Inputs(cfg, engine.Runtime{})
	if err != nil || string(before) != string(after) {
		t.Fatalf("protection changed access inputs: %v", err)
	}
	cfg.Clusters[0].Follows = "source"
	plan, err := engine.Plan(context.Background(), []engine.Step{postgresAccess{}}, cfg, engine.Runtime{}, engine.Options{JournalPath: filepath.Join(t.TempDir(), "journal.json")})
	if err != nil || plan.WouldApply != 0 || plan.Steps[0].Reason != "follower" {
		t.Fatalf("follower access was not skipped: %+v, %v", plan, err)
	}
}
