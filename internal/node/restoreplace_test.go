package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/steps"
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

// samplePath is a concrete path a bundle spec matches.
func samplePath(s masterbackup.Spec) string {
	switch s.Path {
	case masterbackup.HostKeyGlob:
		return "/etc/ssh/ssh_host_ed25519_key"
	case masterbackup.HostKeyPubGlob:
		return "/etc/ssh/ssh_host_ed25519_key.pub"
	case masterbackup.ACMEGlob:
		return steps.ACMEAccountKeyPath
	}
	return s.Path
}

// TestPlacementsMatchTheWriters: every bundled path has a placement, and it
// is the owner, group and mode the file's own writer gives it (the steps'
// scripts included), so a restored file is what apply expects.
func TestPlacementsMatchTheWriters(t *testing.T) {
	want := map[string]placement{
		masterbackup.ClusterPath:            {"root", "root", 0o640},
		masterbackup.NodePath:               {"root", "root", 0o640},
		masterbackup.OperatorsPath:          {"root", "root", 0o640},
		masterbackup.RecordsPath:            {"root", "root", 0o600},
		masterbackup.ProtectPath:            {"root", "root", 0o600},
		masterbackup.WGKeyPath:              {"root", "root", 0o600},
		"/etc/ssh/ssh_host_ed25519_key":     {"root", "root", 0o600},
		"/etc/ssh/ssh_host_ed25519_key.pub": {"root", "root", 0o644},
		masterbackup.TLSCertPath:            {"root", "root", 0o644},
		masterbackup.TLSKeyPath:             {"root", "ssl-cert", 0o640},
		steps.ACMEAccountKeyPath:            {"root", "root", 0o600},
	}
	specs := masterbackup.Specs()
	if len(specs) != len(want) {
		t.Fatalf("%d bundle specs, %d expected placements: add the new path here and to placements", len(specs), len(want))
	}
	for _, s := range specs {
		p := samplePath(s)
		got, ok := placementOf(p)
		if !ok {
			t.Errorf("%s has no placement", p)
			continue
		}
		if got != want[p] {
			t.Errorf("%s placed as %s:%s %04o, want %s:%s %04o", p, got.owner, got.group, got.mode, want[p].owner, want[p].group, want[p].mode)
		}
	}
	if _, ok := placementOf("/etc/vaire/runtime/backup.env"); ok {
		t.Error("a path outside the bundle has a placement")
	}

	// The writers in Go use the same constants placements does; the steps'
	// scripts spell them out.
	if steps.TLSKeyOwner != "root" || steps.TLSKeyGroup != "ssl-cert" || steps.TLSCertOwner != "root" || steps.TLSCertGroup != "root" {
		t.Error("the TLS owners changed: update tls.sh and this test")
	}
	tls := steps.Script("tls")
	for _, line := range []string{
		fmt.Sprintf("install -d -m %04o %s", steps.TLSDirMode, steps.TLSDir),
		fmt.Sprintf("install -d -m %04o %s", steps.ACMEDirMode, steps.ACMEDir),
		fmt.Sprintf("chown %s:%s %s", steps.TLSKeyOwner, steps.TLSKeyGroup, steps.TLSKeyPath),
		fmt.Sprintf("chmod %04o %s", steps.TLSKeyMode, steps.TLSKeyPath),
		fmt.Sprintf("chown %s:%s %s", steps.TLSCertOwner, steps.TLSCertGroup, steps.TLSCertPath),
		fmt.Sprintf("chmod %04o %s", steps.TLSCertMode, steps.TLSCertPath),
	} {
		if !strings.Contains(tls, line) {
			t.Errorf("tls.sh lacks %q", line)
		}
	}
	wg := steps.Script("wireguard-key")
	for _, line := range []string{
		fmt.Sprintf(`install -d -m %04o "$(dirname "${WG_PRIVATE_KEY_PATH}")"`, steps.WGDirMode),
		fmt.Sprintf(`chmod %04o "${tmp}"`, steps.WGPrivateKeyMode),
	} {
		if !strings.Contains(wg, line) {
			t.Errorf("wireguard-key.sh lacks %q", line)
		}
	}

	// The directories as their owners create them.
	if placeDirs[config.Dir] != 0o755 || placeDirs[steps.TLSDir] != 0o755 {
		t.Error("/etc/vaire and /etc/vaire/tls must be 0755: postgres reads the certificate through them")
	}
	for _, d := range nodeDirs {
		if m, ok := placeDirs[d.path]; ok && m != d.mode {
			t.Errorf("%s placed %04o, selfcheck makes it %04o", d.path, m, d.mode)
		}
	}
	for _, s := range specs {
		dir := path.Dir(samplePath(s))
		if _, ok := placeDirs[dir]; !ok {
			t.Errorf("%s's directory %s has no mode in placeDirs", samplePath(s), dir)
		}
	}
}

// TestPlacedAsResolvesNames: placement uses the table's names, never the
// manifest's, and a group this host lacks is root until its step makes it.
func TestPlacedAsResolvesNames(t *testing.T) {
	owner, group, mode, err := placedAs(masterbackup.ClusterPath)
	if err != nil || owner != "root" || group != "root" || mode != config.FileMode {
		t.Errorf("cluster.toml placed as %s:%s %04o, %v", owner, group, mode, err)
	}
	_, group, mode, err = placedAs(masterbackup.TLSKeyPath)
	if err != nil || mode != steps.TLSKeyMode {
		t.Fatalf("server.key placed %04o, %v", mode, err)
	}
	if _, lookErr := user.LookupGroup(steps.TLSKeyGroup); lookErr == nil && group != steps.TLSKeyGroup {
		t.Errorf("server.key's group is %s on a host with %s", group, steps.TLSKeyGroup)
	} else if lookErr != nil && group != "root" {
		t.Errorf("server.key's group is %s on a host without %s", group, steps.TLSKeyGroup)
	}
	if _, _, _, err := placedAs("/etc/passwd"); err == nil {
		t.Error("a path without a placement was placed")
	}
}

// TestPlaceDirGivesEachDirectoryItsMode is the live DR blocker: the ACME
// key sorts first, and MkdirAll made /etc/vaire 0700 with /etc/vaire/acme.
func TestPlaceDirGivesEachDirectoryItsMode(t *testing.T) {
	root := t.TempDir()
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	for _, d := range []string{steps.ACMEDir, steps.TLSDir, config.Dir, "/etc/wireguard", "/var/lib/vaire", "/etc/ssh"} {
		if err := placeDir(root, d); err != nil {
			t.Fatal(err)
		}
	}
	for dir, want := range placeDirs {
		info, err := os.Stat(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %04o, want %04o", dir, got, want)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "/etc")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("/etc is not 0755: %v", err)
	}
}
