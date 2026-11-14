package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
)

func TestAccessShowRealMaterialisationInspection(t *testing.T) {
	o, root := materialisationFixture(t)
	oldRecords, oldInspect := accessLoadRecords, accessInspect
	t.Cleanup(func() { accessLoadRecords, accessInspect = oldRecords, oldInspect })
	accessLoadRecords = func() (backup.Records, error) { return backup.Records{}, nil }
	calls := 0
	accessInspect = func(context.Context, config.ClusterSpec) (steps.AccessCoverage, error) {
		calls++
		return steps.AccessCoverage{Status: "installed"}, nil
	}
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: o.Target}}}
	cfg.Server.DataRoot = root
	env := &Env{Cluster: cfg, Req: rpc.Request{Params: []byte(`{"name":"target"}`)}}
	check := func(state, coverage string, wantCalls int) {
		t.Helper()
		res, err := handleDBAccessShow(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		data := res.Data.(databaseAccess)
		if data.State != state || data.Coverage.Status != coverage || calls != wantCalls {
			t.Fatalf("state=%s coverage=%s calls=%d", data.State, data.Coverage.Status, calls)
		}
	}
	check("", "installed", 1)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	check("materialisation_incomplete", "unknown", 1)
	path, _ := materialisationPath(o.Target)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("materialisation_state_unknown", "unknown", 1)
	if w := unfollowAccessNotice(context.Background(), cfg, cfg.Clusters[0]); w == nil || w.Code != "access_coverage_unknown" || calls != 1 {
		t.Fatalf("unfollow notice: %+v", w)
	}
}

func TestApplyAccessTokensAreSeparate(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "other"}, {Name: "replica", Follows: "app"}}}
	tokens := applyTokens(cfg, nil)
	if !slices.Contains(tokens, "app/access") || slices.Contains(tokens, "replica/access") {
		t.Fatalf("access tokens: %v", tokens)
	}
	req := rpc.Request{Method: "apply", Confirm: "app/access", Confirms: []string{"other/restart"}}
	if err := checkConfirm(methods()["apply"], cfg, req); err != nil {
		t.Fatal(err)
	}
	access := accessConfirmation(cfg, req)
	restart := restartConfirmation(cfg, req)
	if !access["app"] || access["other"] || restart.Covers("app") || !restart.Covers("other") {
		t.Fatalf("access %v restart %+v", access, restart)
	}
	if checkConfirm(methods()["apply"], cfg, rpc.Request{Method: "apply", Confirm: "unknown/access"}) == nil {
		t.Fatal("unknown access token accepted")
	}
}

func TestDBCreateDryRunDispatchIsReadOnly(t *testing.T) {
	m := methods()["db.create"]
	dry, live := rpc.Request{DryRun: true}, rpc.Request{}
	if m.lock(dry) != engine.LockShared || m.runLog(dry) {
		t.Fatal("dry-run create takes a write lock or writes a run log")
	}
	if m.lock(live) != engine.LockExclusive || !m.runLog(live) {
		t.Fatal("live create lost its exclusive lock or run log")
	}
}

type typedFailureStep struct{}

func (typedFailureStep) ID() string                     { return "cpu-test-v1" }
func (typedFailureStep) Describe(config.Cluster) string { return "test" }
func (typedFailureStep) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte("test"), nil
}
func (typedFailureStep) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{}, nil
}
func (typedFailureStep) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	return engine.ApplyResult{}, output.NewError(output.CodeStepFailed, "policy not enforced", output.ExitStep, "retry apply").
		WithDetails(map[string]any{"reason": "cpu_policy_not_enforced", "effective_known": false})
}

func TestTypedStepFailureSurvivesJobOutcome(t *testing.T) {
	_, _, err := engine.Apply(context.Background(), []engine.Step{typedFailureStep{}}, config.Cluster{}, engine.Runtime{},
		engine.Options{JournalPath: filepath.Join(t.TempDir(), "journal.json")})
	var cli *output.CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("not a typed failure: %v", err)
	}
	rec := finishedJobRecord(engine.JobRecord{ID: "job", Job: "test"}, engine.JobFailed, "failed", nil, err)
	_, err = jobOutcome(rec)
	if !errors.As(err, &cli) {
		t.Fatalf("job lost typed failure: %v", err)
	}
	details, ok := cli.Details.(map[string]any)
	if !ok || cli.Code != output.CodeStepFailed || cli.ExitCode != output.ExitStep || cli.Hint != "retry apply" ||
		details["reason"] != "cpu_policy_not_enforced" || details["effective_known"] != false {
		t.Fatalf("job failure details lost: %+v", cli)
	}
}

func TestAccessShowCoverageStates(t *testing.T) {
	oldRecords, oldMaterialisation, oldInspect := accessLoadRecords, accessMaterialisationState, accessInspect
	t.Cleanup(func() {
		accessLoadRecords, accessMaterialisationState, accessInspect = oldRecords, oldMaterialisation, oldInspect
	})
	accessLoadRecords = func() (backup.Records, error) { return backup.Records{}, nil }
	accessMaterialisationState = func(config.Cluster, string) (bool, error) { return false, nil }
	calls := 0
	accessInspect = func(_ context.Context, spec config.ClusterSpec) (steps.AccessCoverage, error) {
		calls++
		if spec.DatabaseName() != "app" {
			t.Errorf("wrong database: %+v", spec)
		}
		return steps.AccessCoverage{Status: "installed"}, nil
	}
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "replica", Follows: "app"}}}
	env := &Env{Cluster: cfg, Req: rpc.Request{Params: []byte(`{"name":"replica"}`)}}
	res, err := handleDBAccessShow(context.Background(), env)
	data := res.Data.(databaseAccess)
	if err != nil || data.Coverage.Status != "installed" || data.Source != "app" || data.Owner != "app_owner" || calls != 1 {
		t.Fatalf("follower: %+v %v", data, err)
	}
	accessMaterialisationState = func(config.Cluster, string) (bool, error) { return true, nil }
	res, err = handleDBAccessShow(context.Background(), env)
	data = res.Data.(databaseAccess)
	if err != nil || data.State != "materialisation_incomplete" || data.Coverage.Status != "unknown" || calls != 1 {
		t.Fatalf("incomplete: %+v %v", data, err)
	}
	accessMaterialisationState = func(config.Cluster, string) (bool, error) { return false, errors.New("unavailable") }
	res, err = handleDBAccessShow(context.Background(), env)
	data = res.Data.(databaseAccess)
	if err != nil || data.Coverage.Status != "unknown" || data.State != "materialisation_state_unknown" || calls != 1 {
		t.Fatalf("unknown: %+v %v", data, err)
	}
	accessLoadRecords = func() (backup.Records, error) {
		return backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"replica": {}}}, nil
	}
	accessMaterialisationState = func(config.Cluster, string) (bool, error) { return true, nil }
	res, err = handleDBAccessShow(context.Background(), env)
	data = res.Data.(databaseAccess)
	if err != nil || data.State != "materialisation_incomplete" || calls != 1 {
		t.Fatalf("incomplete precedence: %+v %v", data, err)
	}
	accessMaterialisationState = func(config.Cluster, string) (bool, error) { return false, nil }
	res, err = handleDBAccessShow(context.Background(), env)
	data = res.Data.(databaseAccess)
	if err != nil || data.State != "awaiting_recovery" || data.Coverage.Status != "unknown" {
		t.Fatalf("awaiting: %+v %v", data, err)
	}
}

func TestUnfollowAccessNoticeNeverWidens(t *testing.T) {
	oldMaterialisation, oldInspect := accessMaterialisationState, accessInspect
	t.Cleanup(func() { accessMaterialisationState, accessInspect = oldMaterialisation, oldInspect })
	spec := config.ClusterSpec{Name: "app"}
	for _, status := range []string{"installed", "pending", "unknown"} {
		t.Run(status, func(t *testing.T) {
			calls := 0
			accessMaterialisationState = func(config.Cluster, string) (bool, error) { return false, nil }
			accessInspect = func(context.Context, config.ClusterSpec) (steps.AccessCoverage, error) {
				calls++
				return steps.AccessCoverage{Status: status}, nil
			}
			warning := unfollowAccessNotice(context.Background(), config.Cluster{}, spec)
			if calls != 1 || (warning == nil) != (status == "installed") {
				t.Fatalf("coverage %s, calls %d, warning %+v", status, calls, warning)
			}
		})
	}
	accessMaterialisationState = func(config.Cluster, string) (bool, error) { return false, errors.New("unknown ownership") }
	accessInspect = func(context.Context, config.ClusterSpec) (steps.AccessCoverage, error) {
		t.Fatal("queried an unverified materialisation")
		return steps.AccessCoverage{}, nil
	}
	if got := unfollowAccessNotice(context.Background(), config.Cluster{}, spec); got == nil || got.Code != "access_coverage_unknown" {
		t.Fatalf("unverified target coverage: %+v", got)
	}
}
