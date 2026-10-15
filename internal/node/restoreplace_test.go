package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/masterbackup"
	"golang.org/x/crypto/ssh"
)

func hostKeyPair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(sk, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), ssh.MarshalAuthorizedKey(sshPub)
}

func TestRestoreFiles(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-abc123"}, {Name: "other", Stanza: "other-def456"}}
	records := backup.Records{
		Leftovers:      []backup.Leftover{{Database: "app", Name: "app__pre_restore_20261014T100000Z"}},
		Restores:       map[string]backup.Restore{"other": {Database: "other", Phase: backup.PhaseReady}},
		Drilling:       map[string]string{"app": "app__drill"},
		DroppedStanzas: []backup.DroppedStanza{{Database: "old", Stanza: "old-999999", ExpiresAt: "2026-11-01T00:00:00Z"}},
		Timelines:      map[string]backup.TimelineSwitch{"app": {Stanza: "app-abc123", Timeline: "3"}},
	}
	recRaw, _ := json.Marshal(records)
	protect, _ := json.Marshal(protectState{Secret: "JBSWY3DP", LastStep: 1234, Failures: 4, LockedUntil: time.Now().Add(time.Hour)}) //nolint:gosec // G117: a test secret.
	priv, pub := hostKeyPair(t)
	b := masterbackup.Bundle{Files: []masterbackup.File{
		masterbackup.NewFile(masterbackup.ClusterPath, "root", "root", 0o640, []byte("old\n")),
		masterbackup.NewFile(masterbackup.RecordsPath, "root", "root", 0o600, recRaw),
		masterbackup.NewFile(masterbackup.ProtectPath, "root", "root", 0o600, protect),
		masterbackup.NewFile("/etc/ssh/ssh_host_ed25519_key", "root", "root", 0o600, priv),
		masterbackup.NewFile("/etc/ssh/ssh_host_ed25519_key.pub", "root", "root", 0o644, pub),
	}}
	files, pruned, err := restoreFiles(b, cfg, "new\n", time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]masterbackup.File{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	if string(byPath[masterbackup.ClusterPath].Data) != "new\n" {
		t.Error("cluster.toml isn't the operator's")
	}
	var got backup.Records
	if err := json.Unmarshal(byPath[masterbackup.RecordsPath].Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Leftovers) != 0 || len(got.Restores) != 0 || len(got.Drilling) != 0 {
		t.Errorf("lost-disk records kept: %+v", got)
	}
	if len(got.DroppedStanzas) != 1 || got.Timelines["app"].Timeline != "3" {
		t.Errorf("durable records lost: %+v", got)
	}
	if !got.IsAwaiting("app") || !got.IsAwaiting("other") || got.AwaitingRecovery["app"].Stanza != "app-abc123" {
		t.Errorf("awaiting recovery: %+v", got.AwaitingRecovery)
	}
	if len(pruned) != 3 {
		t.Errorf("pruned %+v, want the leftover, the open restore and the drill", pruned)
	}
	var st protectState
	if err := json.Unmarshal(byPath[masterbackup.ProtectPath].Data, &st); err != nil {
		t.Fatal(err)
	}
	if st.Secret != "JBSWY3DP" || st.LastStep != 1234 || st.Failures != 0 || !st.LockedUntil.IsZero() {
		t.Errorf("protect state %+v", st)
	}

	b.Files[3].Data = []byte("not a key")
	if _, _, err := restoreFiles(b, cfg, "new\n", time.Now()); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("a bad host key was placed: %v", err)
	}
}

func TestRestoreFilesWithoutRecords(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-abc123"}}
	files, _, err := restoreFiles(masterbackup.Bundle{}, cfg, "x", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != masterbackup.RecordsPath || !strings.Contains(string(files[0].Data), "awaiting_recovery") {
		t.Fatalf("files %+v", files)
	}
}

func TestStepsViewKeepsTheRecovering(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app"}, {Name: "other"}}
	recs := backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"app": {}, "other": {}}}
	if got := withoutAwaiting(cfg, recs, ""); len(got.Clusters) != 0 {
		t.Errorf("view %v", got.Clusters)
	}
	got := withoutAwaiting(cfg, recs, "app")
	if len(got.Clusters) != 1 || got.Clusters[0].Name != "app" {
		t.Errorf("view keeping app: %v", got.Clusters)
	}
	if len(cfg.Clusters) != 2 {
		t.Error("the view changed the configuration it was given")
	}
}

func TestRefuseAwaiting(t *testing.T) {
	recs := backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"app": {}}}
	if err := refuseAwaitingIn(map[string]any{"database": "app"}, recs, "a restore"); err == nil {
		t.Error("a restore of a database awaiting recovery was accepted")
	}
	if err := refuseAwaitingIn(map[string]any{"db": "other"}, recs, "a backup"); err != nil {
		t.Errorf("another database was refused: %v", err)
	}
	for _, m := range []string{"db.drop", "config.set", "db.recover", "db.list", "status"} {
		if _, refused := awaitingRefused[m]; refused {
			t.Errorf("%s is refused for databases awaiting recovery", m)
		}
	}
}

func TestRecoverTokenAndSplitBrain(t *testing.T) {
	if tok := recoverToken(config.Cluster{}, []byte(`{"database":"app"}`)); tok != "" {
		t.Errorf("latest needs no token, got %q", tok)
	}
	if tok := recoverToken(config.Cluster{}, []byte(`{"database":"app","time":"2026-10-15T10:00:00Z"}`)); tok != "app/recover" {
		t.Errorf("token %q", tok)
	}
	if !recoverAtTime([]byte(`{"database":"app","time":"x"}`)) || recoverAtTime([]byte(`{"database":"app"}`)) {
		t.Error("recoverAtTime")
	}
	fence := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	if writtenAfterFence(time.Time{}, fence) || writtenAfterFence(fence.Add(-time.Hour), fence) || writtenAfterFence(fence.Add(30*time.Second), fence) {
		t.Error("an old segment counted as written after the fence")
	}
	if !writtenAfterFence(fence.Add(5*time.Minute), fence) {
		t.Error("a new segment wasn't caught")
	}
	if scaffoldSteps("app")[1] != "postgres-cluster-v1@app" {
		t.Errorf("scaffold steps %v", scaffoldSteps("app"))
	}
}
