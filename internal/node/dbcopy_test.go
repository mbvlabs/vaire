package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

const copyTestJobID = "01J0000000000000000000TEST"

type copyTestNode struct {
	t     *testing.T
	cfg   config.Cluster
	store state.FileStore
	dir   string
}

func copyTestConfig(t *testing.T) config.Cluster {
	t.Helper()
	cfg := config.DefaultCluster()
	cfg.ClusterName, cfg.Domain = "prod", "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: config.DefaultCentralWG}}
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Node: "db1", Port: 5433, Stanza: "app-3f9a1c", Size: "small", StatementTimeout: "60s"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test configuration: %v", err)
	}
	return cfg
}

func copyRoleSecrets(name string) map[string]string {
	out := map[string]string{}
	for _, role := range steps.DatabaseRoles {
		out[steps.SecretDatabaseRole(name, role)] = name + "-" + role + "-password"
	}
	return out
}

func copyTestCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "vaire copy test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// A URL source has no CA form, so the CLI sends the certificate content in
// the separate copy.source_ca secret. Both source forms converge on the same
// validated backup.Source, and the CA is carried into the job spec's
// allowlisted secrets so the detached job can reach the source.
func TestCopySourceSecretsConvergeWithSeparateCA(t *testing.T) {
	ca := string(copyTestCA(t))
	url := "postgresql://source_user:source-password@db.example.com:5433/app?sslmode=verify-full" //nolint:gosec // G101: a test fixture, not a credential.
	fromURL, err := copySourceFromSecrets(map[string]string{
		CopySourceURLSecret: url,
		CopySourceCASecret:  ca,
	})
	if err != nil {
		t.Fatalf("URL source: %v", err)
	}
	spec := CopySourceSecret{
		Host: "db.example.com", Port: 5433, Database: "app", User: "source_user",
		Password: "source-password", TLSMode: backup.SourceTLSVerifyFull,
	}
	raw, err := json.Marshal(spec) //nolint:gosec // G117: the password is this test's source secret.
	if err != nil {
		t.Fatal(err)
	}
	fromFields, err := copySourceFromSecrets(map[string]string{
		CopySourceSpecSecret: string(raw),
		CopySourceCASecret:   ca,
	})
	if err != nil {
		t.Fatalf("field source: %v", err)
	}
	for name, src := range map[string]backup.Source{"url": fromURL, "fields": fromFields} {
		if src.Host != "db.example.com" || src.Port != 5433 || src.Database != "app" ||
			src.User != "source_user" || src.TLSMode != backup.SourceTLSVerifyFull {
			t.Fatalf("%s source = %+v", name, src)
		}
		if string(src.CACert()) != ca {
			t.Fatalf("%s source did not carry the CA", name)
		}
	}
	if fromURL.RedactedURL() != fromFields.RedactedURL() {
		t.Fatalf("the forms do not converge: %s vs %s", fromURL.RedactedURL(), fromFields.RedactedURL())
	}

	if _, err := copySourceFromSecrets(map[string]string{
		CopySourceSpecSecret: string(raw),
		CopySourceCASecret:   ca,
	}); err != nil {
		// The spec itself has no CACertPEM, so this is the accepted form.
		t.Fatal(err)
	}
	spec.CACertPEM = ca
	withBoth, _ := json.Marshal(spec) //nolint:gosec // G117: the password is this test's source secret.
	if _, err := copySourceFromSecrets(map[string]string{
		CopySourceSpecSecret: string(withBoth),
		CopySourceCASecret:   ca,
	}); err == nil {
		t.Fatal("two source CA forms were accepted")
	}

	out, err := copyJobSecrets(map[string]string{
		CopySourceURLSecret:                        url,
		CopySourceCASecret:                         ca,
		steps.SecretDatabaseRole("app", "migrate"): "m",
		steps.SecretDatabaseRole("app", "app"):     "a",
		steps.SecretDatabaseRole("app", "ro"):      "r",
	}, "app")
	if err != nil {
		t.Fatalf("copyJobSecrets: %v", err)
	}
	if out[CopySourceCASecret] != ca {
		t.Fatal("the job spec lost the source CA")
	}
	if out[CopySourceURLSecret] != url {
		t.Fatal("the job spec lost the source URL")
	}
}

func (n *copyTestNode) saved() config.Cluster {
	n.t.Helper()
	cfg, _, err := n.store.Load()
	if err != nil {
		n.t.Fatal(err)
	}
	return cfg
}

func (n *copyTestNode) workspaceRoot() string { return filepath.Join(n.dir, "copies") }

// newCopyTestNode installs the production lifecycle around a scratch
// cluster.toml, with every external boundary replaced by a recording fake.
func newCopyTestNode(t *testing.T) *copyTestNode {
	t.Helper()
	cfg := copyTestConfig(t)
	dir := t.TempDir()
	n := &copyTestNode{t: t, cfg: cfg, store: state.FileStore{Path: filepath.Join(dir, "cluster.toml")}, dir: dir}
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.Save(raw); err != nil {
		t.Fatal(err)
	}
	host := hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}

	oldFollowerState, oldCopyState := followerState, copyState
	oldDetect, oldStarts := detectHost, addDatabaseCheckStarts
	oldInventory, oldConf, oldRecords := copyClusterInventory, copyConfDir, copyLoadRecords
	oldOpen, oldOwner, oldRoot := copyOpenWorkspace, copyWorkspaceOwner, copyRootDir
	oldTools, oldSpace := copyCheckTools, copyCheckCopySpace
	oldPreflight, oldExport, oldSelect := copyPreflightSource, copyExportSource, copySelectArchiveTOC
	oldPlan, oldAdd, oldProvision := copyExtensionPlan, copyAddDatabase, copyProvision
	oldFirstBackup, oldRestore := copyFirstBackup, copyRestoreStrict
	oldPolicy, oldGrants, oldVerify, oldAnalyze, oldActual := copyRestorePolicy, copyRunGrants, copyVerifyObjects, copyAnalyze, copyTargetExtensions
	oldTargetState, oldBefore, oldBeforeSave, oldStartJob := copyReadTargetState, copyBeforeProvision, copyBeforeSave, copyStartJob
	oldClusterExists, oldConfDir := createClusterExists, createConfDir

	followerState = func() state.Store { return n.store }
	copyState = func() state.Store { return n.store }
	detectHost = func(string) (hostinfo.Info, error) { return host, nil }
	addDatabaseCheckStarts = func(config.Cluster, config.ClusterSpec, hostinfo.Info, int) error { return nil }
	copyClusterInventory = func(context.Context) (map[string]string, error) { return map[string]string{"app": "18"}, nil }
	copyConfDir = func(name string) string { return filepath.Join(dir, "etc", "postgresql", "18", name) }
	copyLoadRecords = func() (backup.Records, error) { return backup.Records{}, nil }
	copyWorkspaceOwner = func() (backup.WorkspaceOwner, error) { return backup.CurrentWorkspaceOwner(), nil }
	copyRootDir = n.workspaceRoot()
	copyOpenWorkspace = func(id string) (*backup.CopyWorkspace, error) {
		if err := os.MkdirAll(n.workspaceRoot(), 0o711); err != nil { //nolint:gosec // G301: the copy root needs postgres traverse permission; job dirs stay private.
			return nil, err
		}
		return backup.NewCopyWorkspace(n.workspaceRoot(), id, backup.CurrentWorkspaceOwner(), nil)
	}
	copyCheckTools = func() error { return nil }
	copyCheckCopySpace = func(*backup.CopyWorkspace, string, int64) ([]backup.SpaceNeed, error) {
		return []backup.SpaceNeed{{Path: "test", Need: 1, Free: 2}}, nil
	}
	sourceExtensions := []backup.SourceExtension{
		{Name: "pg_trgm", Version: "1.6", Schema: "public"},
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
	}
	copyPreflightSource = func(ctx context.Context, exec backup.CopyExecutor, stepID string, report engine.Reporter, src backup.Source, ws *backup.CopyWorkspace) (backup.SourcePreflight, error) {
		return backup.SourcePreflight{ServerVersion: 180000, ServerVersionText: "18.0", Database: "app", SizeBytes: 1024,
			ConnectOK: true, CatalogOK: true, TLSMode: backup.SourceTLSVerifyFull, Extensions: sourceExtensions}, nil
	}
	copyExportSource = func(context.Context, backup.CopyExecutor, string, engine.Reporter, backup.Source, *backup.CopyWorkspace) (backup.SourceArchive, error) {
		return backup.SourceArchive{Size: 1024, TOC: backup.ArchiveTOC{Database: "app"}}, nil
	}
	copySelectArchiveTOC = func(context.Context, backup.CopyExecutor, string, engine.Reporter, *backup.CopyWorkspace, backup.SourceArchive, backup.SelectionOptions) (*backup.TOCSelection, error) {
		return &backup.TOCSelection{Lines: []string{"1;"}}, nil
	}
	copyExtensionPlan = func(source []backup.SourceExtension) ([]backup.SourceExtension, []string, []backup.ExtensionMatch, error) {
		plan := []backup.SourceExtension{
			{Name: "pg_stat_statements", Version: "1.10", Schema: "public"},
			{Name: "pg_trgm", Version: "1.6", Schema: "public"},
			{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
		}
		return plan, []string{"pg_stat_statements", "pg_trgm"}, []backup.ExtensionMatch{{Source: sourceExtensions[0], Destination: plan[1]}, {Source: sourceExtensions[1], Destination: plan[2]}}, nil
	}
	copyProvision = func(_ context.Context, _ *Env, cfg config.Cluster, _ []byte, _ config.ClusterSpec) (config.ClusterSpec, []output.Warning, error) {
		created, ok := cfg.ClusterByName("copyapp")
		if !ok {
			return config.ClusterSpec{}, nil, errors.New("the fake provisioner received no target")
		}
		return created, nil, nil
	}
	copyFirstBackup = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string) *output.Warning {
		return nil
	}
	copyRestoreStrict = func(context.Context, backup.CopyExecutor, string, engine.Reporter, *backup.CopyWorkspace, backup.StrictRestoreOptions) error {
		return nil
	}
	copyRestorePolicy = func(context.Context, pgLocal, config.ClusterSpec) error { return nil }
	copyRunGrants = func(context.Context, pgLocal, string) error { return nil }
	copyVerifyObjects = func(context.Context, pgLocal, string) error { return nil }
	copyAnalyze = func(context.Context, pgLocal, string) error { return nil }
	copyTargetExtensions = func(context.Context, pgLocal, string) ([]backup.SourceExtension, error) {
		return []backup.SourceExtension{
			{Name: "pg_stat_statements", Version: "1.10", Schema: "public"},
			{Name: "pg_trgm", Version: "1.6", Schema: "public"},
			{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
		}, nil
	}
	copyReadTargetState = func(_ context.Context, cfg config.Cluster, name string) copyTargetState {
		_, inConfig := cfg.ClusterByName(name)
		return copyTargetState{Name: name, InConfig: inConfig, Cluster: inConfig, Database: inConfig,
			DataDirectory: inConfig, ConfigDirectory: inConfig, Known: true}
	}
	copyBeforeProvision = func() {}
	copyBeforeSave = func() {}
	copyStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
		return "", errors.New("the test did not expect a job start")
	}
	createClusterExists = func(context.Context, string) bool { return false }
	createConfDir = func(name string) string { return filepath.Join(dir, "etc", "postgresql", "18", name) }
	forgetClusterJournal = func(string, string) error { return nil }

	t.Cleanup(func() {
		followerState, copyState = oldFollowerState, oldCopyState
		detectHost, addDatabaseCheckStarts = oldDetect, oldStarts
		copyClusterInventory, copyConfDir, copyLoadRecords = oldInventory, oldConf, oldRecords
		copyOpenWorkspace, copyWorkspaceOwner, copyRootDir = oldOpen, oldOwner, oldRoot
		copyCheckTools, copyCheckCopySpace = oldTools, oldSpace
		copyPreflightSource, copyExportSource, copySelectArchiveTOC = oldPreflight, oldExport, oldSelect
		copyExtensionPlan, copyAddDatabase, copyProvision = oldPlan, oldAdd, oldProvision
		copyFirstBackup, copyRestoreStrict = oldFirstBackup, oldRestore
		copyRestorePolicy, copyRunGrants, copyVerifyObjects, copyAnalyze, copyTargetExtensions = oldPolicy, oldGrants, oldVerify, oldAnalyze, oldActual
		copyReadTargetState, copyBeforeProvision, copyBeforeSave, copyStartJob = oldTargetState, oldBefore, oldBeforeSave, oldStartJob
		createClusterExists, createConfDir = oldClusterExists, oldConfDir
	})
	return n
}

// pgLocal aliases pg.Local so the test fakes stay readable without importing
// pg under a name the production file already uses.
type pgLocal = pg.Local

func newCopyLeaseWorkspace(t *testing.T, root, id, kind string, hold bool) (string, *copyLease) {
	t.Helper()
	ws, err := backup.NewCopyWorkspace(root, id, backup.CurrentWorkspaceOwner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	leaseFile, err := ws.Create(backup.CopyWorkspaceLeaseName)
	if err != nil {
		t.Fatal(err)
	}
	token := "token-" + id
	if _, err := leaseFile.WriteString(token + "\n"); err != nil {
		t.Fatal(err)
	}
	identity, err := backup.CopyWorkspaceLeaseIdentity(leaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile(backup.CopyWorkspaceMarkerName,
		[]byte(kind+"\n"+id+"\n"+identity+"\n"+token+"\n")); err != nil {
		t.Fatal(err)
	}
	lease := &copyLease{file: leaseFile, path: ws.FilePath(backup.CopyWorkspaceLeaseName), identity: identity, token: token}
	if hold {
		if err := syscall.Flock(int(leaseFile.Fd()), syscall.LOCK_SH); err != nil {
			t.Fatal(err)
		}
	} else if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
	return ws.Dir, lease
}

func copyTestJobEnv(t *testing.T, p dbCopyParams, secrets map[string]string) *jobEnv {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return &jobEnv{ID: copyTestJobID, Job: "db-copy", Params: raw, Secrets: secrets,
		Report: func(output.Event) {}}
}

func copyTestSecrets(name string) map[string]string {
	out := copyRoleSecrets(name)
	out[CopySourceURLSecret] = "postgresql://deploy:source-password@db.example.com:5432/app"
	return out
}

func copyErrorDetails(t *testing.T, err error) map[string]any {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("not a CLI error: %v", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	return details
}

func copyWorkspaceGone(t *testing.T, n *copyTestNode, id string) {
	t.Helper()
	path := filepath.Join(n.workspaceRoot(), id)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace %s still exists: %v", path, err)
	}
}

func TestDBCopyTakesExclusiveLocksInOrder(t *testing.T) {
	cfg := copyTestConfig(t)
	var mu sync.Mutex
	var got []string
	modes := map[string]engine.LockMode{}
	locks := jobLockers{
		node: func(_ context.Context, mode engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
			mu.Lock()
			defer mu.Unlock()
			got, modes["node"] = append(got, "node"), mode
			return func() {}, nil
		},
		heavyIO: func(context.Context, string, string) (engine.Release, error) {
			mu.Lock()
			defer mu.Unlock()
			got, modes["heavy"] = append(got, "heavy"), engine.LockExclusive
			return func() {}, nil
		},
		cluster: func(_ context.Context, cluster string, mode engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
			mu.Lock()
			defer mu.Unlock()
			got, modes["cluster "+cluster] = append(got, "cluster "+cluster), mode
			return func() {}, nil
		},
	}
	env := &jobEnv{ID: copyTestJobID, Job: "db-copy", Cluster: cfg, Params: json.RawMessage(`{"name":"copyapp"}`)}
	release, busy, err := takeJobLocks(context.Background(), env, locks)
	if err != nil || busy != nil {
		t.Fatalf("locks: %v %v", err, busy)
	}
	release()
	want := []string{"node", "heavy", "cluster copyapp"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lock order = %v, want %v", got, want)
	}
	for _, key := range want {
		if modes[key] != engine.LockExclusive {
			t.Errorf("%s mode = %q, want exclusive", key, modes[key])
		}
	}
}

func TestDBCopySameTargetIsExclusive(t *testing.T) {
	cfg := copyTestConfig(t)
	locks := fileJobLockers(t)
	env := func() *jobEnv {
		return &jobEnv{ID: copyTestJobID, Job: "db-copy", Cluster: cfg, Params: json.RawMessage(`{"name":"copyapp"}`)}
	}
	release, busy, err := takeJobLocks(context.Background(), env(), locks)
	if err != nil || busy != nil {
		t.Fatalf("the first copy's locks: %v %v", err, busy)
	}
	defer release()
	if _, busy, err = takeJobLocks(context.Background(), env(), locks); err != nil {
		t.Fatalf("the second copy's locks: %v", err)
	}
	if busy == nil {
		t.Fatal("a second copy of the same unconfigured target took its locks")
	}
}

func TestDBCopySkippedResultSaysNoImportRan(t *testing.T) {
	res := map[string]any{"job": "db-copy", "skipped": "lock_busy"}
	copySkippedFields("db-copy", res)
	if res["phase"] != copyPhasePreflight || res["import_ran"] != false {
		t.Fatalf("skipped result = %v", res)
	}
	other := map[string]any{}
	copySkippedFields("db-fork", other)
	if len(other) != 0 {
		t.Fatalf("a non-copy job was changed: %v", other)
	}
}

func TestDBCopyUnconfiguredTargetStillTakesClusterLock(t *testing.T) {
	cfg := copyTestConfig(t)
	env := &jobEnv{Job: "db-copy", Cluster: cfg, Params: json.RawMessage(`{"name":"not_configured"}`)}
	got := jobClusters(env)
	if len(got) != 1 || got[0] != "not_configured" {
		t.Fatalf("jobClusters = %v", got)
	}
}

func TestDBCopyPreflightDoesNotReserve(t *testing.T) {
	n := newCopyTestNode(t)
	params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
	env := &Env{Cluster: n.cfg, Req: rpc.Request{RequestID: copyTestJobID, Params: params, Secrets: copyTestSecrets("copyapp")},
		Report: func(output.Event) {}}
	res, err := handleDBCopyPreflight(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := res.Data.(map[string]any)
	target, _ := data["target"].(map[string]any)
	spec, _ := target["database"].(config.ClusterSpec)
	if spec.Name != "copyapp" {
		t.Fatalf("target = %+v", spec)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); ok {
		t.Fatal("preflight reserved the target in cluster.toml")
	}
	if _, err := os.Lstat(filepath.Join(n.cfg.Server.DataRoot, "copyapp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight created a data directory: %v", err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "source-password") {
		t.Fatal("the preflight result carries the source password")
	}
	copyWorkspaceGone(t, n, copyTestJobID)
}

func TestDBCopyPreflightRequiresSource(t *testing.T) {
	n := newCopyTestNode(t)
	params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
	env := &Env{Cluster: n.cfg, Req: rpc.Request{RequestID: copyTestJobID, Params: params, Secrets: copyRoleSecrets("copyapp")}, Report: func(output.Event) {}}
	_, err := handleDBCopyPreflight(context.Background(), env)
	if err == nil {
		t.Fatal("a preflight without a source succeeded")
	}
	if details := copyErrorDetails(t, err); details["reason"] != "source_required" {
		t.Fatalf("details = %v", details)
	}
}

func TestDBCopyRejectsTargetCollisions(t *testing.T) {
	n := newCopyTestNode(t)
	ctx := context.Background()

	withTarget := n.cfg
	withTarget.Clusters = append(withTarget.Clusters, config.ClusterSpec{Name: "copyapp", Node: "db1", Port: 5434, Stanza: "copyapp-abcdef", Size: "small", StatementTimeout: "60s"})
	if err := checkCopyTargetAbsent(ctx, withTarget, "copyapp", false); err == nil || copyErrorDetails(t, err)["reason"] != "database_exists" {
		t.Fatalf("configured target: %v", err)
	}

	oldInventory := copyClusterInventory
	copyClusterInventory = func(context.Context) (map[string]string, error) { return map[string]string{"copyapp": "17"}, nil }
	t.Cleanup(func() { copyClusterInventory = oldInventory })
	err := checkCopyTargetAbsent(ctx, n.cfg, "copyapp", false)
	if err == nil || copyErrorDetails(t, err)["reason"] != "target_cluster_exists" {
		t.Fatalf("other-major cluster: %v", err)
	}
	copyClusterInventory = oldInventory

	if err := os.Symlink(filepath.Join(n.dir, "elsewhere"), filepath.Join(n.cfg.Server.DataRoot, "copyapp")); err != nil {
		t.Fatal(err)
	}
	err = checkCopyTargetAbsent(ctx, n.cfg, "copyapp", false)
	if err == nil || copyErrorDetails(t, err)["reason"] != "target_path_exists" {
		t.Fatalf("dangling data symlink: %v details=%v", err, copyErrorDetails(t, err))
	}
	if err := os.Remove(filepath.Join(n.cfg.Server.DataRoot, "copyapp")); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(copyConfDir("copyapp"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = checkCopyTargetAbsent(ctx, n.cfg, "copyapp", false)
	if err == nil || copyErrorDetails(t, err)["reason"] != "target_path_exists" {
		t.Fatalf("configuration directory: %v", err)
	}

	if err := os.RemoveAll(copyConfDir("copyapp")); err != nil {
		t.Fatal(err)
	}
	oldRecords := copyLoadRecords
	copyLoadRecords = func() (backup.Records, error) {
		return backup.Records{Restores: map[string]backup.Restore{"copyapp": {Database: "copyapp"}}}, nil
	}
	t.Cleanup(func() { copyLoadRecords = oldRecords })
	err = checkCopyTargetAbsent(ctx, n.cfg, "copyapp", false)
	if err == nil || copyErrorDetails(t, err)["reason"] != "target_reserved" {
		t.Fatalf("reserved target: %v", err)
	}
	copyLoadRecords = oldRecords
}

func TestDBCopyExportFailureLeavesNoTarget(t *testing.T) {
	n := newCopyTestNode(t)
	copyExportSource = func(context.Context, backup.CopyExecutor, string, engine.Reporter, backup.Source, *backup.CopyWorkspace) (backup.SourceArchive, error) {
		return backup.SourceArchive{}, output.NewError(output.CodePrecondition, "the source export failed", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "source_export_failed", "output_tail": []string{"ERROR: secret row data"}})
	}
	env := copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp"))
	_, err := jobCopy(context.Background(), env)
	if err == nil {
		t.Fatal("the job succeeded")
	}
	details := copyErrorDetails(t, err)
	if details["phase"] != copyPhaseExporting || details["target_present"] != false || details["import_ran"] != false {
		t.Fatalf("details = %v", details)
	}
	if _, ok := details["output_tail"]; ok {
		t.Fatal("raw subprocess output reached the job outcome")
	}
	if _, ok := n.saved().ClusterByName("copyapp"); ok {
		t.Fatal("the failed export added the target to cluster.toml")
	}
	copyWorkspaceGone(t, n, copyTestJobID)
}

func TestDBCopyLateCollisionRechecked(t *testing.T) {
	n := newCopyTestNode(t)
	copyReadTargetState = copyTargetStateOf
	copyBeforeProvision = func() {
		if err := os.MkdirAll(filepath.Join(n.cfg.Server.DataRoot, "copyapp"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp"))
	_, err := jobCopy(context.Background(), env)
	if err == nil {
		t.Fatal("the job succeeded despite the late collision")
	}
	details := copyErrorDetails(t, err)
	if details["phase"] != copyPhaseExporting || details["target_present"] != true {
		t.Fatalf("details = %v", details)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); ok {
		t.Fatal("the late collision was still provisioned")
	}
	copyWorkspaceGone(t, n, copyTestJobID)
}

func TestDBCopyCollisionBeforeConfigInsertion(t *testing.T) {
	n := newCopyTestNode(t)
	copyReadTargetState = copyTargetStateOf
	copyBeforeSave = func() {
		if err := os.MkdirAll(filepath.Join(n.cfg.Server.DataRoot, "copyapp"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp"))
	_, err := jobCopy(context.Background(), env)
	if err == nil {
		t.Fatal("the job succeeded despite the collision before the config insertion")
	}
	details := copyErrorDetails(t, err)
	if details["phase"] != copyPhaseProvisioning || details["target_present"] != true {
		t.Fatalf("details = %v", details)
	}
	target, _ := details["target"].(copyTargetState)
	if target.InConfig || !target.DataDirectory || !target.Known {
		t.Fatalf("target state = %+v", target)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); ok {
		t.Fatal("the collision still saved the target in cluster.toml")
	}
	copyWorkspaceGone(t, n, copyTestJobID)
}

func TestDBCopyImportFailureLeavesTruthfulPartialTarget(t *testing.T) {
	n := newCopyTestNode(t)
	copyRestoreStrict = func(context.Context, backup.CopyExecutor, string, engine.Reporter, *backup.CopyWorkspace, backup.StrictRestoreOptions) error {
		return output.NewError(output.CodeExternalCommand, "pg_restore failed", output.ExitExternal, "").
			WithDetails(map[string]any{"reason": "restore_failed", "output_tail": []string{"ERROR: COPY failed"}})
	}
	env := copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp"))
	_, err := jobCopy(context.Background(), env)
	if err == nil {
		t.Fatal("the job succeeded")
	}
	details := copyErrorDetails(t, err)
	if details["phase"] != copyPhaseImporting || details["target_present"] != true || details["import_ran"] != true {
		t.Fatalf("details = %v", details)
	}
	if _, ok := details["output_tail"]; ok {
		t.Fatal("raw restore output reached the job outcome")
	}
	if _, ok := n.saved().ClusterByName("copyapp"); !ok {
		t.Fatal("the partial target was removed")
	}
	copyWorkspaceGone(t, n, copyTestJobID)

	// A repeated copy is refused, never replayed into the partial target.
	if err := checkCopyTargetAbsent(context.Background(), n.saved(), "copyapp", false); err == nil || copyErrorDetails(t, err)["reason"] != "database_exists" {
		t.Fatalf("replay was not refused: %v", err)
	}
}

func TestDBCopyFinalizeFailuresLeavePartialTarget(t *testing.T) {
	cases := map[string]struct {
		setup func()
		phase string
	}{
		"policy": {func() {
			copyRestorePolicy = func(context.Context, pgLocal, config.ClusterSpec) error { return errors.New("policy failed") }
		}, copyPhaseFinalizing},
		"grants": {func() {
			copyRunGrants = func(context.Context, pgLocal, string) error { return errors.New("GRANT failed") }
		}, copyPhaseFinalizing},
		"verify": {func() {
			copyVerifyObjects = func(context.Context, pgLocal, string) error {
				return output.NewError(output.CodePrecondition, "ownership verification failed", output.ExitPrecondition, "").
					WithDetails(map[string]any{"reason": "copy_verify_failed", "counts": map[string]int{"object_owner_mismatch": 1}})
			}
		}, copyPhaseFinalizing},
		"analyze": {func() {
			copyAnalyze = func(context.Context, pgLocal, string) error { return errors.New("ANALYZE failed") }
		}, copyPhaseFinalizing},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			n := newCopyTestNode(t)
			tc.setup()
			env := copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp"))
			_, err := jobCopy(context.Background(), env)
			if err == nil {
				t.Fatal("the job succeeded")
			}
			details := copyErrorDetails(t, err)
			if details["phase"] != tc.phase || details["target_present"] != true || details["import_ran"] != true {
				t.Fatalf("details = %v", details)
			}
			if _, ok := n.saved().ClusterByName("copyapp"); !ok {
				t.Fatal("the partial target was removed")
			}
			copyWorkspaceGone(t, n, copyTestJobID)
		})
	}
}

func TestReadCopyJobSpecUnlinkFailureIsFailClosed(t *testing.T) {
	oldDir := copyJobDir
	copyJobDir = t.TempDir()
	t.Cleanup(func() { copyJobDir = oldDir })
	const password = "unlink-secret-password" //nolint:gosec // G101: a test credential, never a real secret.
	spec := jobSpec{Job: "db-copy", Params: json.RawMessage(`{"name":"copyapp"}`), Secrets: map[string]string{"db.copyapp.app": password}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(copyJobDir, copyTestJobID+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(copyJobDir, 0o500); err != nil { //nolint:gosec // G302: a deliberately unwritable directory for the test.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(copyJobDir, 0o700) }) //nolint:gosec // G302: restore the test directory.
	got, err := readCopyJobSpec(copyTestJobID)
	if err == nil {
		t.Fatal("readCopyJobSpec accepted a spec it could not unlink")
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("not a typed error: %v", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["reason"] != "copy_job_spec_unlink_failed" || details["job_id"] != copyTestJobID {
		t.Fatalf("details = %v", details)
	}
	if got.Secrets["db.copyapp.app"] != password {
		t.Fatalf("the parsed spec was lost: %+v", got)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the retained spec disappeared: %v", statErr)
	}
	if redacted := secrets.Default.Redact(password); redacted != secrets.Placeholder {
		t.Fatalf("the decoded password was not registered for redaction: %q", redacted)
	}
}

func TestPruneCopyWorkspacesCleansSpecsWithoutRoot(t *testing.T) {
	oldRoot, oldDir, oldState, oldInactive, oldRemove, oldRead := copyRootDir, copyJobDir, loadJobState, copyWorkspaceInactive, copyRemoveJobSpec, copyReadDir
	copyRootDir = filepath.Join(t.TempDir(), "absent")
	copyJobDir = t.TempDir()
	t.Cleanup(func() {
		copyRootDir, copyJobDir, loadJobState, copyWorkspaceInactive, copyRemoveJobSpec, copyReadDir = oldRoot, oldDir, oldState, oldInactive, oldRemove, oldRead
	})
	const activeID = "01J0000000000000000000ACTV"
	loadJobState = func() (engine.State, error) {
		return engine.State{Jobs: []engine.JobRecord{
			{ID: copyTestJobID, Job: "db-copy", State: engine.JobSucceeded},
			{ID: activeID, Job: "db-copy", State: engine.JobRunning},
		}}, nil
	}
	copyWorkspaceInactive = func(_ context.Context, rec engine.JobRecord) bool { return rec.ID == copyTestJobID }
	inactiveSpec := filepath.Join(copyJobDir, copyTestJobID+".json")
	activeSpec := filepath.Join(copyJobDir, activeID+".json")
	for _, path := range []string{inactiveSpec, activeSpec} {
		if err := os.WriteFile(path, []byte("spec"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	warnings := pruneCopyWorkspaces(context.Background())
	if _, err := os.Lstat(inactiveSpec); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the inactive spec was not removed without a workspace root: %v", err)
	}
	if _, err := os.Lstat(activeSpec); err != nil {
		t.Fatalf("the active spec was removed: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("absent root generated warnings: %v", warnings)
	}

	// An unsafe root must not suppress spec cleanup either.
	rootPath := filepath.Join(t.TempDir(), "copies-file")
	if err := os.WriteFile(rootPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyRootDir = rootPath
	if err := os.WriteFile(inactiveSpec, []byte("spec"), 0o600); err != nil {
		t.Fatal(err)
	}
	warnings = pruneCopyWorkspaces(context.Background())
	if _, err := os.Lstat(inactiveSpec); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the inactive spec was not removed with an unsafe root: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("the unsafe root was not reported")
	}

	// An unreadable root must not suppress spec cleanup either.
	copyRootDir = t.TempDir()
	copyReadDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("root unreadable") }
	if err := os.WriteFile(inactiveSpec, []byte("spec"), 0o600); err != nil {
		t.Fatal(err)
	}
	warnings = pruneCopyWorkspaces(context.Background())
	if _, err := os.Lstat(inactiveSpec); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the inactive spec was not removed with an unreadable root: %v", err)
	}
	foundRootWarning := false
	for _, warning := range warnings {
		foundRootWarning = foundRootWarning || warning.Code == "copy_workspace_prune_failed"
	}
	if !foundRootWarning {
		t.Fatalf("the unreadable root was not reported: %v", warnings)
	}

	// Cleanup failure is retained and reported.
	copyReadDir = oldRead
	copyRootDir = rootPath
	if err := os.WriteFile(inactiveSpec, []byte("spec"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyRemoveJobSpec = func(string) error { return errors.New("spec busy") }
	warnings = pruneCopyWorkspaces(context.Background())
	if _, err := os.Lstat(inactiveSpec); err != nil {
		t.Fatalf("the spec was removed despite the failure: %v", err)
	}
	found := false
	for _, warning := range warnings {
		found = found || warning.Code == "copy_job_spec_cleanup_failed"
	}
	if !found {
		t.Fatalf("cleanup failure warning missing: %v", warnings)
	}
}

func TestDBCopyCleanupFailureIsFatalAfterImport(t *testing.T) {
	n := newCopyTestNode(t)
	oldOwner := copyWorkspaceOwner
	copyWorkspaceOwner = func() (backup.WorkspaceOwner, error) {
		owner := backup.CurrentWorkspaceOwner()
		owner.UID++
		return owner, nil
	}
	t.Cleanup(func() { copyWorkspaceOwner = oldOwner })
	env := copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp"))
	_, err := jobCopy(context.Background(), env)
	if err == nil {
		t.Fatal("a cleanup failure after import was reported as success")
	}
	details := copyErrorDetails(t, err)
	if details["reason"] != "copy_cleanup_failed" || details["imported"] != true ||
		details["target_present"] != true || details["workspace_cleanup"] != "failed" ||
		details["blocked"] != true {
		t.Fatalf("details = %v", details)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); !ok {
		t.Fatal("the imported target was removed")
	}
}

func TestDBCopyBackupWarningIsSeparateFromImportSuccess(t *testing.T) {
	n := newCopyTestNode(t)
	cfg := n.saved()
	cfg.Backup.Provider = "r2"
	cfg.Backup.Bucket, cfg.Backup.Endpoint, cfg.Backup.Prefix = "bucket", "https://example.invalid", "prod"
	cfg.Backup.Region, cfg.Backup.URIStyle = "auto", "path"
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.Save(raw); err != nil {
		t.Fatal(err)
	}
	copyFirstBackup = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string) *output.Warning {
		return &output.Warning{Code: "first_backup_failed", Message: "the first backup failed"}
	}
	env := copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp"))
	res, err := jobCopy(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := res.(map[string]any)
	if data["imported"] != true || data["phase"] != copyPhaseSucceeded {
		t.Fatalf("result = %v", data)
	}
	backupState, _ := data["backup"].(copyBackupState)
	if !backupState.Configured || backupState.Ready || backupState.Warning == nil {
		t.Fatalf("backup state = %+v", backupState)
	}
	warnings, _ := data["warnings"].([]output.Warning)
	found := false
	for _, warning := range warnings {
		found = found || warning.Code == "first_backup_failed"
	}
	if !found {
		t.Fatalf("warnings = %v", warnings)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); !ok {
		t.Fatal("the successful copy is not in cluster.toml")
	}
	copyWorkspaceGone(t, n, copyTestJobID)
}

func TestDBCopySuccess(t *testing.T) {
	n := newCopyTestNode(t)
	var selection backup.SelectionOptions
	copySelectArchiveTOC = func(_ context.Context, _ backup.CopyExecutor, _ string, _ engine.Reporter, _ *backup.CopyWorkspace, _ backup.SourceArchive, opts backup.SelectionOptions) (*backup.TOCSelection, error) {
		selection = opts
		return &backup.TOCSelection{Lines: []string{"1;"}}, nil
	}
	var restore backup.StrictRestoreOptions
	copyRestoreStrict = func(_ context.Context, _ backup.CopyExecutor, _ string, _ engine.Reporter, _ *backup.CopyWorkspace, opts backup.StrictRestoreOptions) error {
		restore = opts
		return nil
	}
	env := copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp"))
	res, err := jobCopy(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := res.(map[string]any)
	if data["imported"] != true || data["phase"] != copyPhaseSucceeded {
		t.Fatalf("result = %v", data)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); !ok {
		t.Fatal("the target is not in cluster.toml")
	}
	if selection.SourceDatabase != "app" {
		t.Fatalf("selection source = %q", selection.SourceDatabase)
	}
	names := make([]string, 0, len(selection.Extensions))
	for _, ext := range selection.Extensions {
		names = append(names, ext.Name)
	}
	if strings.Join(names, ",") != "pg_trgm,plpgsql" {
		t.Fatalf("selection extensions = %v", names)
	}
	if restore.Target != "copyapp" || restore.Owner != "copyapp_owner" {
		t.Fatalf("restore options = %+v", restore)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "source-password") || strings.Contains(string(raw), "deploy:source") {
		t.Fatalf("the job result carries the source password: %s", raw)
	}
	copyWorkspaceGone(t, n, copyTestJobID)
}

func TestDBCopyExtensionFailureDoesNotProvision(t *testing.T) {
	n := newCopyTestNode(t)
	copyExtensionPlan = func([]backup.SourceExtension) ([]backup.SourceExtension, []string, []backup.ExtensionMatch, error) {
		return nil, nil, nil, output.NewError(output.CodePrecondition, "the source extension is missing", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "extension_missing"})
	}
	env := copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp"))
	_, err := jobCopy(context.Background(), env)
	if err == nil {
		t.Fatal("the job succeeded")
	}
	if details := copyErrorDetails(t, err); details["phase"] != copyPhasePreflight || details["target_present"] != false {
		t.Fatalf("details = %v", details)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); ok {
		t.Fatal("an incompatible extension still provisioned the target")
	}
	copyWorkspaceGone(t, n, copyTestJobID)
}

func TestDBCopyDryRunStartsNothing(t *testing.T) {
	n := newCopyTestNode(t)
	started := false
	copyStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
		started = true
		return "", errors.New("unexpected start")
	}
	params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
	env := &Env{Cluster: n.cfg, Req: rpc.Request{DryRun: true, Params: params, Secrets: map[string]string{}}, Report: func(output.Event) {}}
	res, err := handleDBCopy(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("a dry run started a job")
	}
	data, _ := res.Data.(map[string]any)
	if data["dry_run"] != true || data["source_checked"] != false {
		t.Fatalf("result = %v", data)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); ok {
		t.Fatal("a dry run added the target")
	}
}

func TestDBCopyAmbiguousStartReturnsJobIDAndUnknown(t *testing.T) {
	n := newCopyTestNode(t)
	copyStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
		return copyTestJobID, &copyJobStartAmbiguousError{ID: copyTestJobID}
	}
	params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
	env := &Env{Cluster: n.cfg, Req: rpc.Request{RequestID: copyTestJobID, Params: params, Secrets: copyTestSecrets("copyapp")},
		Report: func(output.Event) {}}
	_, err := handleDBCopy(context.Background(), env)
	if err == nil {
		t.Fatal("the ambiguous start was reported as success")
	}
	details := copyErrorDetails(t, err)
	if details["reason"] != JobOutcomeUnknown || details["job_id"] != copyTestJobID ||
		details["credentials_may_be_used"] != true || details["phase"] != "unknown" {
		t.Fatalf("details = %v", details)
	}
}

// A state read or follow failure after the launch was confirmed is an
// observation failure, not proof that the copy failed. The job id and the
// possibly installed credentials must survive, with a truthful target state
// and no source secret in the error.
func TestDBCopyFollowObservationFailureIsUnknown(t *testing.T) {
	assertUnknown := func(t *testing.T, err error) {
		t.Helper()
		details := copyErrorDetails(t, err)
		if details["reason"] != JobOutcomeUnknown || details["job_id"] != copyTestJobID ||
			details["credentials_may_be_used"] != true || details["observation_failed"] != true ||
			details["phase"] != "unknown" || details["start"] != "confirmed" ||
			details["import_outcome"] != "unknown" {
			t.Fatalf("details = %v", details)
		}
		if details["target_present"] != false {
			t.Fatalf("an unobserved copy claimed a target: %v", details)
		}
		raw, marshalErr := json.Marshal(err)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, secret := range []string{"source-password", "deploy:source"} {
			if strings.Contains(string(raw), secret) || strings.Contains(err.Error(), secret) {
				t.Fatalf("the observation error carries a source secret")
			}
		}
	}

	t.Run("initial state read", func(t *testing.T) {
		n := newCopyTestNode(t)
		copyStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
			return copyTestJobID, nil
		}
		oldLoad := loadJobState
		loadJobState = func() (engine.State, error) { return engine.State{}, errors.New("state unavailable") }
		t.Cleanup(func() { loadJobState = oldLoad })
		params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
		env := &Env{Cluster: n.cfg, Req: rpc.Request{RequestID: copyTestJobID, Params: params, Secrets: copyTestSecrets("copyapp")},
			Report: func(output.Event) {}}
		_, err := handleDBCopy(context.Background(), env)
		if err == nil {
			t.Fatal("the observation failure was reported as success")
		}
		assertUnknown(t, err)
	})

	t.Run("poll state read", func(t *testing.T) {
		n := newCopyTestNode(t)
		copyStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
			return copyTestJobID, nil
		}
		oldLoad, oldPoll := loadJobState, jobFollowPoll
		jobFollowPoll = time.Millisecond
		reads := 0
		loadJobState = func() (engine.State, error) {
			reads++
			switch reads {
			case 1:
				return engine.State{}, nil // the start's workspace-spec pruning
			case 2:
				return engine.State{Jobs: []engine.JobRecord{{ID: copyTestJobID, Job: "db-copy", State: engine.JobRunning}}}, nil
			default:
				return engine.State{}, errors.New("state read failed while the job runs")
			}
		}
		t.Cleanup(func() { loadJobState, jobFollowPoll = oldLoad, oldPoll })
		params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
		env := &Env{Cluster: n.cfg, Req: rpc.Request{RequestID: copyTestJobID, Params: params, Secrets: copyTestSecrets("copyapp")},
			Report: func(output.Event) {}}
		_, err := handleDBCopy(context.Background(), env)
		if err == nil {
			t.Fatal("the poll observation failure was reported as success")
		}
		assertUnknown(t, err)
		if reads < 3 {
			t.Fatalf("the follower read state %d times, want an initial and a poll read", reads)
		}
	})

	t.Run("observer cancellation while running", func(t *testing.T) {
		n := newCopyTestNode(t)
		copyStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
			return copyTestJobID, nil
		}
		oldLoad := loadJobState
		loadJobState = func() (engine.State, error) {
			return engine.State{Jobs: []engine.JobRecord{{ID: copyTestJobID, Job: "db-copy", State: engine.JobRunning}}}, nil
		}
		t.Cleanup(func() { loadJobState = oldLoad })
		params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
		env := &Env{Cluster: n.cfg, Req: rpc.Request{RequestID: copyTestJobID, Params: params, Secrets: copyTestSecrets("copyapp")},
			Report: func(output.Event) {}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := handleDBCopy(ctx, env)
		if err == nil {
			t.Fatal("the observer cancellation was reported as success")
		}
		assertUnknown(t, err)
	})
}

// A job that genuinely recorded a terminal cancellation before any target
// exists stays a definite failure; observer cancellation handling must not
// rewrite it as an unknown running job.
func TestDBCopyRecordedTerminalCancellationStaysDefinite(t *testing.T) {
	n := newCopyTestNode(t)
	copyStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
		return copyTestJobID, nil
	}
	rec := engine.JobRecord{
		ID: copyTestJobID, Job: "db-copy", State: engine.JobFailed,
		Error: &output.ErrorBody{
			Code: output.CodeError, Message: "the database copy was interrupted",
			Details: map[string]any{
				"reason": "cancelled", "phase": "exporting", "import_ran": false,
				"imported": false, "target_present": false, "database": "copyapp",
			},
		},
	}
	oldLoad := loadJobState
	loadJobState = func() (engine.State, error) { return engine.State{Jobs: []engine.JobRecord{rec}}, nil }
	t.Cleanup(func() { loadJobState = oldLoad })
	params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
	env := &Env{Cluster: n.cfg, Req: rpc.Request{RequestID: copyTestJobID, Params: params, Secrets: copyTestSecrets("copyapp")},
		Report: func(output.Event) {}}
	_, err := handleDBCopy(context.Background(), env)
	if err == nil {
		t.Fatal("the terminal cancellation was reported as success")
	}
	details := copyErrorDetails(t, err)
	if details["reason"] != "cancelled" || details["phase"] != "exporting" ||
		details["target_present"] != false || details["imported"] != false {
		t.Fatalf("the terminal outcome changed: %v", details)
	}
	if _, ok := details["observation_failed"]; ok {
		t.Fatalf("a recorded terminal cancellation was marked as an observation failure: %v", details)
	}
	if _, ok := details["credentials_may_be_used"]; ok {
		t.Fatalf("a before-target terminal cancellation claimed credentials may be used: %v", details)
	}
}

func TestJobFollowObservationFailureIsTyped(t *testing.T) {
	t.Run("state read", func(t *testing.T) {
		oldLoad := loadJobState
		loadJobState = func() (engine.State, error) { return engine.State{}, errors.New("state unavailable") }
		t.Cleanup(func() { loadJobState = oldLoad })
		params, _ := json.Marshal(map[string]string{"id": copyTestJobID})
		_, err := handleJobFollow(context.Background(), &Env{Req: rpc.Request{Params: params}})
		details := copyErrorDetails(t, err)
		if details["reason"] != JobObservationFailed || details["job_id"] != copyTestJobID || details["observation_failed"] != true {
			t.Fatalf("details = %v", details)
		}
	})

	t.Run("observer cancellation while running", func(t *testing.T) {
		oldLoad := loadJobState
		loadJobState = func() (engine.State, error) {
			return engine.State{Jobs: []engine.JobRecord{{ID: copyTestJobID, Job: "db-copy", State: engine.JobRunning}}}, nil
		}
		t.Cleanup(func() { loadJobState = oldLoad })
		params, _ := json.Marshal(map[string]string{"id": copyTestJobID})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := handleJobFollow(ctx, &Env{Req: rpc.Request{Params: params}})
		details := copyErrorDetails(t, err)
		if details["reason"] != JobObservationFailed || details["job_id"] != copyTestJobID || details["observation_failed"] != true {
			t.Fatalf("details = %v", details)
		}
	})
}

func TestDBCopyStartFailureLeavesNoTarget(t *testing.T) {
	n := newCopyTestNode(t)
	copyStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
		return "", errors.New("systemd-run failed")
	}
	params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
	env := &Env{Cluster: n.cfg, Req: rpc.Request{RequestID: copyTestJobID, Params: params, Secrets: copyTestSecrets("copyapp")}, Report: func(output.Event) {}}
	_, err := handleDBCopy(context.Background(), env)
	if err == nil {
		t.Fatal("the start failure was ignored")
	}
	if _, ok := n.saved().ClusterByName("copyapp"); ok {
		t.Fatal("a failed job start added the target")
	}
}

func TestDBCopyPreflightCleanupFailureIsReported(t *testing.T) {
	n := newCopyTestNode(t)
	copyOpenWorkspace = func(id string) (*backup.CopyWorkspace, error) {
		if err := os.MkdirAll(n.workspaceRoot(), 0o711); err != nil { //nolint:gosec // G301: the copy root needs postgres traverse permission; job dirs stay private.
			return nil, err
		}
		return backup.NewCopyWorkspace(n.workspaceRoot(), id, backup.CurrentWorkspaceOwner(), nil)
	}
	oldOwner := copyWorkspaceOwner
	copyWorkspaceOwner = func() (backup.WorkspaceOwner, error) {
		return backup.WorkspaceOwner{UID: backup.CurrentWorkspaceOwner().UID + 1, GID: backup.CurrentWorkspaceOwner().GID}, nil
	}
	t.Cleanup(func() { copyWorkspaceOwner = oldOwner })
	params, _ := json.Marshal(dbCopyParams{Name: "copyapp", Size: "small"})
	env := &Env{Cluster: n.cfg, Req: rpc.Request{RequestID: copyTestJobID, Params: params, Secrets: copyTestSecrets("copyapp")}, Report: func(output.Event) {}}
	if _, err := handleDBCopyPreflight(context.Background(), env); err == nil {
		t.Fatal("an unverifiable cleanup was hidden")
	}
}

func TestProvisionDatabaseRollsBackBeforeClusterExists(t *testing.T) {
	n := newCopyTestNode(t)
	cfg := n.saved()
	previous, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	spec := config.ClusterSpec{Name: "copyapp", Node: "db1", Port: 5434, Stanza: "copyapp-abcdef", Size: "small", StatementTimeout: "60s", Extensions: []string{"pg_stat_statements"}}
	proposed := cfg
	proposed.Clusters = append(proposed.Clusters, spec)
	raw, err := proposed.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.Save(raw); err != nil {
		t.Fatal(err)
	}
	oldKernel, oldExists := createApplyKernel, createClusterExists
	createApplyKernel = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		return engine.ApplyData{}, nil, errors.New("the kernel step failed")
	}
	createClusterExists = func(context.Context, string) bool { return false }
	t.Cleanup(func() { createApplyKernel, createClusterExists = oldKernel, oldExists })

	env := &Env{Cluster: proposed, Req: rpc.Request{Secrets: copyRoleSecrets("copyapp")}}
	if _, _, err := provisionDatabase(context.Background(), env, proposed, previous, spec, createOptions{deferInitialBackup: true}); err == nil {
		t.Fatal("the provisioning failure was ignored")
	}
	if _, ok := n.saved().ClusterByName("copyapp"); ok {
		t.Fatal("a cluster that was never created left its config entry")
	}
}

func TestProvisionDatabaseKeepsConfigAfterClusterExists(t *testing.T) {
	n := newCopyTestNode(t)
	cfg := n.saved()
	previous, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	spec := config.ClusterSpec{Name: "copyapp", Node: "db1", Port: 5434, Stanza: "copyapp-abcdef", Size: "small", StatementTimeout: "60s", Extensions: []string{"pg_stat_statements"}}
	proposed := cfg
	proposed.Clusters = append(proposed.Clusters, spec)
	raw, err := proposed.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.Save(raw); err != nil {
		t.Fatal(err)
	}
	oldKernel, oldExists := createApplyKernel, createClusterExists
	createApplyKernel = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		return engine.ApplyData{}, nil, errors.New("a later cluster step failed")
	}
	createClusterExists = func(context.Context, string) bool { return true }
	t.Cleanup(func() { createApplyKernel, createClusterExists = oldKernel, oldExists })

	env := &Env{Cluster: proposed, Req: rpc.Request{Secrets: copyRoleSecrets("copyapp")}}
	_, _, err = provisionDatabase(context.Background(), env, proposed, previous, spec, createOptions{deferInitialBackup: true})
	if err == nil {
		t.Fatal("the provisioning failure was ignored")
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || !strings.Contains(cliErr.Hint, "vaire apply --db copyapp") {
		t.Fatalf("the partial-cluster hint is missing: %v", err)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); !ok {
		t.Fatal("a cluster that exists lost its config entry")
	}
}

func TestProvisionDatabaseDefersFirstBackup(t *testing.T) {
	n := newCopyTestNode(t)
	cfg := n.saved()
	previous, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	spec := config.ClusterSpec{Name: "copyapp", Node: "db1", Port: 5434, Stanza: "copyapp-abcdef", Size: "small", StatementTimeout: "60s", Extensions: []string{"pg_stat_statements"}}
	proposed := cfg
	proposed.Clusters = append(proposed.Clusters, spec)
	raw, err := proposed.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.Save(raw); err != nil {
		t.Fatal(err)
	}
	oldKernel, oldNew, oldPoolers := createApplyKernel, createApplyNewCluster, createReloadPoolers
	oldMetrics, oldOthers, oldBackup := createApplyMetrics, createApplyOtherConfigs, createFirstBackup
	createApplyKernel = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		return engine.ApplyData{}, nil, nil
	}
	createApplyNewCluster = func(context.Context, config.Cluster, engine.Runtime, string, string, engine.Reporter) error {
		return nil
	}
	createReloadPoolers = func(context.Context, *Env, config.Cluster) error { return nil }
	createApplyMetrics = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string, string) *output.Warning {
		return nil
	}
	createApplyOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, []string, engine.Options) []output.Warning {
		return nil
	}
	backups := 0
	createFirstBackup = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string) *output.Warning {
		backups++
		return nil
	}
	t.Cleanup(func() {
		createApplyKernel, createApplyNewCluster, createReloadPoolers = oldKernel, oldNew, oldPoolers
		createApplyMetrics, createApplyOtherConfigs, createFirstBackup = oldMetrics, oldOthers, oldBackup
	})
	env := &Env{Cluster: proposed, Req: rpc.Request{Secrets: copyRoleSecrets("copyapp")}}
	if _, _, err := provisionDatabase(context.Background(), env, proposed, previous, spec, createOptions{deferInitialBackup: true}); err != nil {
		t.Fatal(err)
	}
	if backups != 0 {
		t.Fatalf("the deferred backup ran %d times", backups)
	}
	if _, _, err := provisionDatabase(context.Background(), env, proposed, previous, spec, createOptions{}); err != nil {
		t.Fatal(err)
	}
	if backups != 1 {
		t.Fatalf("ordinary provisioning ran %d backups, want 1", backups)
	}
}

func TestCopyPreviewBudgetAndOvercommit(t *testing.T) {
	n := newCopyTestNode(t)
	oldHost := detectHost
	detectHost = func(string) (hostinfo.Info, error) {
		return hostinfo.Info{VCPU: 2, RAMMiB: 1024, DataDisk: hostinfo.Disk{SizeGiB: 100}}, nil
	}
	t.Cleanup(func() { detectHost = oldHost })
	env := &Env{Cluster: n.cfg}
	if _, _, _, err := copyPreviewTarget(env, n.cfg, dbCopyParams{Name: "copyapp", Size: "large"}, []string{"pg_stat_statements"}); err == nil {
		t.Fatal("the overcommitting size was accepted")
	}
	if _, _, warnings, err := copyPreviewTarget(env, n.cfg, dbCopyParams{Name: "copyapp", Size: "large", AllowOvercommit: true}, []string{"pg_stat_statements"}); err != nil {
		t.Fatal(err)
	} else if len(warnings) == 0 {
		t.Fatal("allow-overcommit did not carry the overcommit warning")
	}
}

func TestCopyTargetStateReportsActualArtefacts(t *testing.T) {
	n := newCopyTestNode(t)
	cfg := n.cfg
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: "copyapp", Node: "db1", Port: 5434, Stanza: "copyapp-abcdef", Size: "small", StatementTimeout: "60s"})
	oldInventory := copyClusterInventory
	copyClusterInventory = func(context.Context) (map[string]string, error) {
		return map[string]string{"app": "18"}, nil
	}
	t.Cleanup(func() { copyClusterInventory = oldInventory })

	st := copyTargetStateOf(context.Background(), cfg, "copyapp")
	if !st.InConfig || !st.Known || st.Cluster || st.Database || !st.Present() {
		t.Fatalf("state = %+v", st)
	}
	if err := os.MkdirAll(cfg.Server.DataDirectory("copyapp"), 0o700); err != nil {
		t.Fatal(err)
	}
	st = copyTargetStateOf(context.Background(), cfg, "copyapp")
	if !st.DataDirectory || !st.Present() {
		t.Fatalf("data-directory state = %+v", st)
	}
	copyClusterInventory = func(context.Context) (map[string]string, error) {
		return nil, errors.New("pg_lsclusters is unavailable")
	}
	st = copyTargetStateOf(context.Background(), cfg, "copyapp")
	if st.Known {
		t.Fatalf("an unreadable inventory was reported as known: %+v", st)
	}
	if !st.Present() {
		t.Fatalf("known artefacts were lost: %+v", st)
	}
}

func TestCopyJobLivenessIsConservative(t *testing.T) {
	oldFacts, oldScan, oldProcess, oldAlive := copyReadUnitFacts, copyScanWorker, copyProcessState, copyJobProcessAlive
	t.Cleanup(func() {
		copyReadUnitFacts, copyScanWorker, copyProcessState, copyJobProcessAlive = oldFacts, oldScan, oldProcess, oldAlive
	})
	rec := engine.JobRecord{ID: copyTestJobID, Job: "db-copy", Unit: "vaire-job-" + copyTestJobID + ".service",
		State: engine.JobRunning, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	cases := []struct {
		name  string
		facts copyUnitFactsValue
		err   error
		scan  bool
		ok    bool
		want  copyLiveness
	}{
		{"active", copyUnitFactsValue{LoadState: "loaded", ActiveState: "active"}, nil, false, true, copyLivenessActive},
		{"activating", copyUnitFactsValue{LoadState: "loaded", ActiveState: "activating"}, nil, false, true, copyLivenessActive},
		{"reloading", copyUnitFactsValue{LoadState: "loaded", ActiveState: "reloading"}, nil, false, true, copyLivenessActive},
		{"deactivating", copyUnitFactsValue{LoadState: "loaded", ActiveState: "deactivating"}, nil, false, true, copyLivenessActive},
		{"query_error", copyUnitFactsValue{}, errors.New("systemctl unavailable"), false, true, copyLivenessUnknown},
		{"unknown_state", copyUnitFactsValue{LoadState: "loaded", ActiveState: "maintenance"}, nil, false, true, copyLivenessUnknown},
		{"masked", copyUnitFactsValue{LoadState: "masked", ActiveState: "inactive"}, nil, false, true, copyLivenessUnknown},
		{"inactive_worker_present", copyUnitFactsValue{LoadState: "loaded", ActiveState: "inactive"}, nil, true, true, copyLivenessActive},
		{"inactive_scan_unknown", copyUnitFactsValue{LoadState: "loaded", ActiveState: "inactive"}, nil, false, false, copyLivenessUnknown},
		{"inactive", copyUnitFactsValue{LoadState: "loaded", ActiveState: "inactive"}, nil, false, true, copyLivenessInactive},
		{"not_found", copyUnitFactsValue{LoadState: "not-found", ActiveState: "inactive"}, nil, false, true, copyLivenessInactive},
		{"not_found_worker_present", copyUnitFactsValue{LoadState: "not-found", ActiveState: "inactive"}, nil, true, true, copyLivenessActive},
		{"failed", copyUnitFactsValue{LoadState: "loaded", ActiveState: "failed"}, nil, false, true, copyLivenessInactive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copyReadUnitFacts = func(context.Context, string) (copyUnitFactsValue, error) { return tc.facts, tc.err }
			copyScanWorker = func(string) (bool, bool) { return tc.scan, tc.ok }
			if got := copyJobLiveness(context.Background(), rec); got != tc.want {
				t.Fatalf("liveness = %v, want %v", got, tc.want)
			}
		})
	}

	copyJobProcessAlive = func(int, string) bool { return true }
	live := rec
	live.PID = 12345
	if got := copyJobLiveness(context.Background(), live); got != copyLivenessActive {
		t.Fatalf("a live recorded PID was reported as %v", got)
	}
	copyJobProcessAlive = func(int, string) bool { return false }
	copyScanWorker = func(string) (bool, bool) { return false, true }

	copyReadUnitFacts = func(context.Context, string) (copyUnitFactsValue, error) {
		return copyUnitFactsValue{LoadState: "loaded", ActiveState: "inactive", MainPID: 42}, nil
	}
	copyJobProcessAlive = func(int, string) bool { return true }
	if got := copyJobLiveness(context.Background(), rec); got != copyLivenessActive {
		t.Fatalf("a live MainPID was reported as %v", got)
	}
	copyJobProcessAlive = func(int, string) bool { return false }
	if got := copyJobLiveness(context.Background(), rec); got != copyLivenessInactive {
		t.Fatalf("a dead MainPID without a worker was reported as %v", got)
	}

	copyReadUnitFacts = func(context.Context, string) (copyUnitFactsValue, error) {
		return copyUnitFactsValue{LoadState: "loaded", ActiveState: "inactive", ControlPID: 43}, nil
	}
	copyProcessState = func(int) (bool, bool) { return true, true }
	if got := copyJobLiveness(context.Background(), rec); got != copyLivenessActive {
		t.Fatalf("a live ControlPID was reported as %v", got)
	}
	copyProcessState = func(int) (bool, bool) { return false, true }
	if got := copyJobLiveness(context.Background(), rec); got != copyLivenessInactive {
		t.Fatalf("a dead ControlPID without a worker was reported as %v", got)
	}
	copyProcessState = func(int) (bool, bool) { return false, false }
	if got := copyJobLiveness(context.Background(), rec); got != copyLivenessUnknown {
		t.Fatalf("an indeterminate ControlPID was reported as %v", got)
	}

	// A terminal record never bypasses the liveness check.
	copyReadUnitFacts = func(context.Context, string) (copyUnitFactsValue, error) {
		return copyUnitFactsValue{LoadState: "loaded", ActiveState: "active"}, nil
	}
	terminal := rec
	terminal.State = engine.JobSucceeded
	if copyWorkspaceInactive(context.Background(), terminal) {
		t.Fatal("a terminal record with an active unit was treated as inactive")
	}
	copyReadUnitFacts = func(context.Context, string) (copyUnitFactsValue, error) {
		return copyUnitFactsValue{LoadState: "loaded", ActiveState: "inactive"}, nil
	}
	copyScanWorker = func(string) (bool, bool) { return false, true }
	if !copyWorkspaceInactive(context.Background(), terminal) {
		t.Fatal("an inactive terminal record was not treated as inactive")
	}
}

func TestCopySyntheticCrashDoesNotOverwriteWorkerTerminal(t *testing.T) {
	oldUpdate := putCrashedJobState
	t.Cleanup(func() { putCrashedJobState = oldUpdate })
	rec := engine.JobRecord{ID: copyTestJobID, Job: "db-copy", Unit: "vaire-job-" + copyTestJobID + ".service",
		State: engine.JobRunning, StartedAt: "2026-11-12T00:00:00Z"}
	current := engine.JobRecord{ID: rec.ID, Job: rec.Job, Unit: rec.Unit,
		State: engine.JobSucceeded, StartedAt: rec.StartedAt}
	putCrashedJobState = func(fn func(*engine.State) error) error {
		state := engine.State{Jobs: []engine.JobRecord{current}}
		err := fn(&state)
		current = state.Jobs[0]
		return err
	}
	got, updated := putCrashedJobIfUnfinished(rec)
	if updated || got.State != engine.JobSucceeded {
		t.Fatalf("a worker terminal record was overwritten: %+v updated=%v", got, updated)
	}

	current = rec
	got, updated = putCrashedJobIfUnfinished(rec)
	if !updated || got.State != engine.JobFailed {
		t.Fatalf("a non-terminal copy was not crashed: %+v updated=%v", got, updated)
	}
	if details, _ := got.Error.Details.(map[string]any); details["reason"] != JobOutcomeUnknown {
		t.Fatalf("crash details = %v", got.Error.Details)
	}
}

func TestStartCopyJobAmbiguousKeepsIDAndRemovesSpec(t *testing.T) {
	oldDir, oldUpdate, oldLaunch, oldRemove := copyJobDir, copyUpdateState, copyLaunchJob, copyRemoveJobSpec
	t.Cleanup(func() {
		copyJobDir, copyUpdateState, copyLaunchJob, copyRemoveJobSpec = oldDir, oldUpdate, oldLaunch, oldRemove
	})
	copyJobDir = t.TempDir()
	var state engine.State
	copyUpdateState = func(fn func(*engine.State) error) error {
		state = engine.State{}
		return fn(&state)
	}
	copyLaunchJob = func(context.Context, *Env, string, string, string) error {
		return errors.New("systemd-run failed after acceptance")
	}
	copyRemoveJobSpec = func(id string) error {
		return os.Remove(filepath.Join(copyJobDir, id+".json"))
	}
	env := &Env{Req: rpc.Request{RequestID: copyTestJobID}, Report: func(output.Event) {}}
	id, err := startCopyJob(context.Background(), env, "db-copy", dbCopyParams{Name: "copyapp"}, map[string]string{"db.copyapp.app": "secret"})
	if err == nil || id != copyTestJobID {
		t.Fatalf("id=%q err=%v", id, err)
	}
	var ambiguous *copyJobStartAmbiguousError
	if !errors.As(err, &ambiguous) || ambiguous.SpecErr != nil {
		t.Fatalf("ambiguous error = %#v", err)
	}
	if _, err := os.Lstat(filepath.Join(copyJobDir, id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the unread spec was not removed: %v", err)
	}
	rec, ok := state.Job(id)
	if !ok || rec.Final() || rec.State != engine.JobQueued {
		t.Fatalf("ambiguous launch wrote a terminal/absent record: %+v ok=%v", rec, ok)
	}
}

func TestStartCopyJobPrelaunchRefusalIsDefinite(t *testing.T) {
	oldDir, oldUpdate, oldLaunch, oldRemove := copyJobDir, copyUpdateState, copyLaunchJob, copyRemoveJobSpec
	t.Cleanup(func() {
		copyJobDir, copyUpdateState, copyLaunchJob, copyRemoveJobSpec = oldDir, oldUpdate, oldLaunch, oldRemove
	})
	copyJobDir = t.TempDir()
	launched := false
	copyLaunchJob = func(context.Context, *Env, string, string, string) error {
		launched = true
		return nil
	}
	copyUpdateState = func(func(*engine.State) error) error {
		return errors.New("state unavailable")
	}
	copyRemoveJobSpec = func(id string) error {
		return os.Remove(filepath.Join(copyJobDir, id+".json"))
	}
	env := &Env{Req: rpc.Request{RequestID: copyTestJobID}, Report: func(output.Event) {}}
	id, err := startCopyJob(context.Background(), env, "db-copy", dbCopyParams{Name: "copyapp"}, nil)
	if err == nil || id != "" || launched {
		t.Fatalf("id=%q launched=%v err=%v", id, launched, err)
	}
	if _, err := os.Lstat(filepath.Join(copyJobDir, copyTestJobID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a prelaunch refusal left its spec: %v", err)
	}
}

func TestStartCopyJobAmbiguousReportsSpecCleanupFailure(t *testing.T) {
	oldDir, oldUpdate, oldLaunch, oldRemove := copyJobDir, copyUpdateState, copyLaunchJob, copyRemoveJobSpec
	t.Cleanup(func() {
		copyJobDir, copyUpdateState, copyLaunchJob, copyRemoveJobSpec = oldDir, oldUpdate, oldLaunch, oldRemove
	})
	copyJobDir = t.TempDir()
	copyUpdateState = func(fn func(*engine.State) error) error {
		var st engine.State
		return fn(&st)
	}
	copyLaunchJob = func(context.Context, *Env, string, string, string) error {
		return errors.New("systemd-run failed")
	}
	copyRemoveJobSpec = func(string) error { return errors.New("spec is busy") }
	env := &Env{Req: rpc.Request{RequestID: copyTestJobID}, Report: func(output.Event) {}}
	id, err := startCopyJob(context.Background(), env, "db-copy", dbCopyParams{Name: "copyapp"}, nil)
	var ambiguous *copyJobStartAmbiguousError
	if !errors.As(err, &ambiguous) || id != copyTestJobID {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if ambiguous.SpecErr == nil {
		t.Fatal("the spec cleanup failure was hidden")
	}
}

func TestPruneCopyWorkspaces(t *testing.T) {
	n := newCopyTestNode(t)
	root := n.workspaceRoot()
	if err := os.MkdirAll(root, 0o711); err != nil { //nolint:gosec // G301: the copy root needs postgres traverse permission; job dirs stay private.
		t.Fatal(err)
	}
	copyRootDir = root
	copyWorkspaceOwner = func() (backup.WorkspaceOwner, error) { return backup.CurrentWorkspaceOwner(), nil }

	makeWorkspaceKind := func(id, kind string, hold bool) (string, *os.File) {
		t.Helper()
		ws, err := backup.NewCopyWorkspace(root, id, backup.CurrentWorkspaceOwner(), nil)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := ws.Create(backup.CopyWorkspaceLeaseName)
		if err != nil {
			t.Fatal(err)
		}
		token := "token-" + id
		if _, err := lease.WriteString(token + "\n"); err != nil {
			t.Fatal(err)
		}
		identity, err := backup.CopyWorkspaceLeaseIdentity(lease)
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.WriteFile(backup.CopyWorkspaceMarkerName,
			[]byte(kind+"\n"+id+"\n"+identity+"\n"+token+"\n")); err != nil {
			t.Fatal(err)
		}
		if hold {
			if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_SH); err != nil {
				t.Fatal(err)
			}
		} else if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		if err := ws.Close(); err != nil {
			t.Fatal(err)
		}
		return ws.Dir, lease
	}
	newWorkspace := func(id string) string {
		t.Helper()
		path, _ := makeWorkspaceKind(id, "db-copy", false)
		t.Cleanup(func() { _ = os.RemoveAll(path) })
		return path
	}

	terminal := newWorkspace("01J0000000000000000000TERM")
	abrupt := newWorkspace("01J0000000000000000000ABRT")
	crashed := newWorkspace("01J0000000000000000000CRSH")
	active := newWorkspace("01J0000000000000000000ACTV")
	unknown := newWorkspace("01J0000000000000000000UNKN")
	otherJob := newWorkspace("01J0000000000000000000OTHR")
	unsafe := newWorkspace("01J0000000000000000000UNSF")
	preflightIdle, preflightIdleLease := makeWorkspaceKind("01J0000000000000000000POLD", "db-copy-preflight", false)
	preflightActive, preflightActiveLease := makeWorkspaceKind("01J0000000000000000000PNEW", "db-copy-preflight", true)
	_ = preflightIdleLease.Close()
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(preflightIdle, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(preflightActive, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = preflightActiveLease.Close()
		_ = os.RemoveAll(preflightIdle)
		_ = os.RemoveAll(preflightActive)
	})
	if err := os.Chmod(unsafe, 0o755); err != nil { //nolint:gosec // G302: a deliberately unsafe fixture permission.
		t.Fatal(err)
	}
	outside := filepath.Join(n.cfg.Server.DataRoot, "database-data")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	oldJobDir := copyJobDir
	copyJobDir = t.TempDir()
	t.Cleanup(func() { copyJobDir = oldJobDir })
	if err := os.WriteFile(filepath.Join(copyJobDir, filepath.Base(terminal)+".json"), []byte("spec"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyJobDir, filepath.Base(active)+".json"), []byte("spec"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldState, oldInactive := loadJobState, copyWorkspaceInactive
	loadJobState = func() (engine.State, error) {
		return engine.State{Jobs: []engine.JobRecord{
			{ID: filepath.Base(terminal), Job: "db-copy", State: engine.JobSucceeded},
			{ID: filepath.Base(abrupt), Job: "db-copy", State: engine.JobRunning, PID: 1 << 22},
			{ID: filepath.Base(crashed), Job: "db-copy", State: engine.JobFailed, Error: &output.ErrorBody{Details: map[string]any{"reason": JobOutcomeUnknown}}},
			{ID: filepath.Base(active), Job: "db-copy", State: engine.JobRunning},
			{ID: filepath.Base(otherJob), Job: "db-fork", State: engine.JobSucceeded},
			{ID: filepath.Base(unsafe), Job: "db-copy", State: engine.JobSucceeded},
		}}, nil
	}
	copyWorkspaceInactive = func(_ context.Context, rec engine.JobRecord) bool {
		return rec.State == engine.JobSucceeded || rec.ID == filepath.Base(abrupt) || rec.State == engine.JobFailed
	}
	t.Cleanup(func() { loadJobState, copyWorkspaceInactive = oldState, oldInactive })

	warnings := pruneCopyWorkspaces(context.Background())
	for _, path := range []string{terminal, abrupt, crashed, preflightIdle} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("terminal or inactive workspace %s was not removed: %v", path, err)
		}
	}
	for _, path := range []string{active, unknown, otherJob, preflightActive} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("workspace %s was removed: %v", path, err)
		}
	}
	if _, err := os.Lstat(unsafe); err != nil {
		t.Fatalf("unsafe workspace was removed: %v", err)
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Fatalf("a database directory was touched: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(copyJobDir, filepath.Base(terminal)+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the inactive copy job spec was not removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(copyJobDir, filepath.Base(active)+".json")); err != nil {
		t.Fatalf("the active copy job spec was removed: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("the unsafe workspace was not reported")
	}
}

func TestCopyLeasePruneBusyAndIdentity(t *testing.T) {
	root := t.TempDir()
	const id = "01J0000000000000000000LEAS"
	path, lease := newCopyLeaseWorkspace(t, root, id, "db-copy", true)
	t.Cleanup(func() {
		_ = lease.Close()
		_ = os.RemoveAll(path)
	})
	owner := backup.CurrentWorkspaceOwner()
	if _, err := backup.PruneCopyWorkspace(root, id, owner); !errors.Is(err, backup.ErrCopyWorkspaceBusy) {
		t.Fatalf("active lease prune = %v, want busy", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if removed, err := backup.PruneCopyWorkspace(root, id, owner); err != nil || !removed {
		t.Fatalf("free lease prune = removed %v, err %v", removed, err)
	}

	// A replacement lease inode is not a lock identity handoff.
	path, lease = newCopyLeaseWorkspace(t, root, id, "db-copy", false)
	leasePath := filepath.Join(path, backup.CopyWorkspaceLeaseName)
	if err := os.Remove(leasePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.PruneCopyWorkspace(root, id, owner); err == nil {
		t.Fatal("a replacement lease was accepted")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the replacement workspace was removed: %v", err)
	}
	_ = os.RemoveAll(path)

	// A same-inode token change is not a lock handoff.
	path, lease = newCopyLeaseWorkspace(t, root, id, "db-copy", false)
	leasePath = filepath.Join(path, backup.CopyWorkspaceLeaseName)
	if err := os.WriteFile(leasePath, []byte("different-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.PruneCopyWorkspace(root, id, owner); err == nil {
		t.Fatal("a same-inode lease token change was accepted")
	}
	exec := copyLeaseExecutor{inner: engine.Shell{Secrets: secrets.Default}, lease: lease}
	if err := exec.Exec(context.Background(), "lease", engine.Command{Name: "/bin/true"}, nil); err == nil {
		t.Fatal("the lease wrapper ran after a same-inode token change")
	}
	_ = os.RemoveAll(path)
}

func TestCopyLeaseExecutorFailsClosedOnReplacement(t *testing.T) {
	root := t.TempDir()
	const id = "01J0000000000000000000EXEC"
	path, lease := newCopyLeaseWorkspace(t, root, id, "db-copy", true)
	t.Cleanup(func() {
		_ = lease.Close()
		_ = os.RemoveAll(path)
	})
	exec := copyLeaseExecutor{inner: engine.Shell{Secrets: secrets.Default}, lease: lease}
	if err := exec.Exec(context.Background(), "lease", engine.Command{Name: "/bin/true"}, nil); err != nil {
		t.Fatalf("the lease wrapper could not run: %v", err)
	}
	leasePath := filepath.Join(path, backup.CopyWorkspaceLeaseName)
	if err := os.Remove(leasePath); err != nil {
		t.Fatal(err)
	}
	if err := exec.Exec(context.Background(), "lease", engine.Command{Name: "/bin/true"}, nil); err == nil {
		t.Fatal("the lease wrapper ran with a deleted lease")
	}
	if err := os.WriteFile(leasePath, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Exec(context.Background(), "lease", engine.Command{Name: "/bin/true"}, nil); err == nil {
		t.Fatal("the lease wrapper ran with a replacement lease")
	}
}

func TestCopyLeaseSurvivesParentUntilDescendantExit(t *testing.T) {
	root := t.TempDir()
	const id = "01J0000000000000000000DESC"
	path, lease := newCopyLeaseWorkspace(t, root, id, "db-copy", true)
	t.Cleanup(func() {
		_ = lease.Close()
		_ = os.RemoveAll(path)
	})
	ready := filepath.Join(path, "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec := copyLeaseExecutor{inner: engine.Shell{Secrets: secrets.Default}, lease: lease}
	done := make(chan error, 1)
	go func() {
		done <- exec.Exec(ctx, "lease", engine.Command{Name: "/bin/sh", Args: []string{"-c", "touch " + ready + "; sleep 5"}}, nil)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease descendant did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.PruneCopyWorkspace(root, id, backup.CurrentWorkspaceOwner()); !errors.Is(err, backup.ErrCopyWorkspaceBusy) {
		t.Fatalf("prune while the descendant held the lease = %v, want busy", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lease descendant did not stop")
	}
	if removed, err := backup.PruneCopyWorkspace(root, id, backup.CurrentWorkspaceOwner()); err != nil || !removed {
		t.Fatalf("prune after the descendant exited = removed %v, err %v", removed, err)
	}
}

func TestCopyLeaseInterleavedAcquisitionGapFailsClosed(t *testing.T) {
	root := t.TempDir()
	gateDir := t.TempDir()
	const id = "01J0000000000000000000GAPP"
	path, lease := newCopyLeaseWorkspace(t, root, id, "db-copy", true)
	t.Cleanup(func() {
		_ = lease.Close()
		_ = os.RemoveAll(path)
	})
	oldTool := copyLeaseTool
	t.Cleanup(func() { copyLeaseTool = oldTool })
	fakeFlock := filepath.Join(gateDir, "flock-gate")
	ready := filepath.Join(gateDir, "ready")
	proceed := filepath.Join(gateDir, "proceed")
	script := "#!/bin/sh\nprintf ready > " + ready + "\nwhile [ ! -f " + proceed + " ]; do sleep 0.01; done\nexec " + oldTool + " \"$@\"\n"
	if err := os.WriteFile(fakeFlock, []byte(script), 0o700); err != nil { //nolint:gosec // G302: an executable test fixture.
		t.Fatal(err)
	}
	copyLeaseTool = fakeFlock
	executed := filepath.Join(gateDir, "executed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec := copyLeaseExecutor{inner: engine.Shell{Secrets: secrets.Default}, lease: lease}
	done := make(chan error, 1)
	go func() {
		done <- exec.Exec(ctx, "lease", engine.Command{Name: "/bin/sh", Args: []string{"-c", "touch " + executed}}, nil)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease wrapper did not reach the acquisition gap")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Release the parent after the wrapper opened/verified the lease but
	// before it acquired the shared lock: prune may delete it.
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if removed, err := backup.PruneCopyWorkspace(root, id, backup.CurrentWorkspaceOwner()); err != nil || !removed {
		t.Fatalf("prune in the acquisition gap = removed %v, err %v", removed, err)
	}
	if err := os.WriteFile(proceed, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lease wrapper did not finish")
	}
	if _, err := os.Lstat(executed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the wrapped command executed after workspace deletion: %v", err)
	}
}

func TestPruneCopyWorkspaceRefusesSymlinkAndWrongOwner(t *testing.T) {
	n := newCopyTestNode(t)
	root := n.workspaceRoot()
	if err := os.MkdirAll(root, 0o711); err != nil { //nolint:gosec // G301: the copy root needs postgres traverse permission; job dirs stay private.
		t.Fatal(err)
	}
	target := filepath.Join(n.dir, "do-not-delete")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "01J0000000000000000000LINK")); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.PruneCopyWorkspace(root, "01J0000000000000000000LINK", backup.CurrentWorkspaceOwner()); err == nil {
		t.Fatal("a symlink workspace was accepted")
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("the symlink target was removed: %v", err)
	}
	wrong := filepath.Join(root, "01J0000000000000000000OWNR")
	if err := os.Mkdir(wrong, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := backup.CurrentWorkspaceOwner()
	owner.UID++
	if _, err := backup.PruneCopyWorkspace(root, "01J0000000000000000000OWNR", owner); err == nil {
		t.Fatal("a workspace with the wrong owner was accepted")
	}
	if _, err := os.Lstat(wrong); err != nil {
		t.Fatalf("the wrong-owner workspace was removed: %v", err)
	}
}
