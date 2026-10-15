package cli

import (
	"errors"
	"slices"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/masterbackup"
)

func TestRecoverOrder(t *testing.T) {
	dbs := []databaseInfo{
		{Name: "big", State: stateAwaitingRecovery, SizeBytes: 9000},
		{Name: "small", State: stateAwaitingRecovery, SizeBytes: 10},
		{Name: "vital", State: stateAwaitingRecovery, SizeBytes: 5000, Protected: true},
		{Name: "running", State: "running"},
		{Name: "mid", State: stateAwaitingRecovery, SizeBytes: 100},
	}
	if got := recoverOrder(dbs, nil); !slices.Equal(got, []string{"vital", "small", "mid", "big"}) {
		t.Errorf("order %v", got)
	}
	if got := recoverOrder(dbs, []string{"big"}); !slices.Equal(got, []string{"big", "vital", "small", "mid"}) {
		t.Errorf("named first: %v", got)
	}
}

func TestPickEndpoint(t *testing.T) {
	for name, c := range map[string]struct {
		stored  string
		answers [][]string
		want    string
	}{
		"moved":          {"203.0.113.10", [][]string{{"198.51.100.7"}, {"198.51.100.7"}}, "198.51.100.7"},
		"unchanged":      {"203.0.113.10", [][]string{{"203.0.113.10"}, {"203.0.113.10"}}, ""},
		"disagree":       {"203.0.113.10", [][]string{{"198.51.100.7"}, {"198.51.100.8"}}, ""},
		"one answered":   {"203.0.113.10", [][]string{nil, {"198.51.100.7"}}, "198.51.100.7"},
		"none answered":  {"203.0.113.10", [][]string{nil, nil}, ""},
		"several record": {"203.0.113.10", [][]string{{"198.51.100.7", "198.51.100.8"}}, ""},
	} {
		if got := pickEndpoint(c.stored, c.answers...); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestParseEndpoint(t *testing.T) {
	for in, want := range map[string]struct {
		addr string
		port int
		ok   bool
	}{
		"198.51.100.7":        {"198.51.100.7", 0, true},
		"198.51.100.7:51821":  {"198.51.100.7", 51821, true},
		"[2001:db8::7]:51820": {"2001:db8::7", 51820, true},
		"2001:db8::7":         {"2001:db8::7", 0, true},
		"db1.example.com":     {"", 0, false},
		"198.51.100.7:99999":  {"", 0, false},
	} {
		addr, port, err := parseEndpoint(in)
		if (err == nil) != want.ok || addr != want.addr || port != want.port {
			t.Errorf("%s: %s %d %v", in, addr, port, err)
		}
	}
}

func TestPickBundleName(t *testing.T) {
	at := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	a := masterbackup.Name(at, 1, "aaa")
	b := masterbackup.Name(at.Add(time.Hour), 1, "bbb")
	c := masterbackup.Name(at.Add(2*time.Hour), 1, "ccc")
	list := []backup.MasterBundle{{Name: c, Epoch: 1, Complete: false}, {Name: b, Epoch: 1, Complete: true}, {Name: a, Epoch: 1, Complete: true}}
	p, older, err := pickBundleName(list, "")
	if err != nil || p.Name != b || older != a {
		t.Fatalf("newest: %s older %s (%v)", p.Name, older, err)
	}
	p, _, err = pickBundleName(list, at.Add(30*time.Minute).Format(time.RFC3339))
	if err != nil || p.Name != a {
		t.Fatalf("by time: %s (%v)", p.Name, err)
	}
	if _, _, err := pickBundleName(list, c); err == nil {
		t.Fatal("an incomplete bundle was picked by name")
	}
	var ce *output.CLIError
	if _, _, err := pickBundleName(nil, ""); !errors.As(err, &ce) || ce.Code != output.CodeBundleNotFound {
		t.Fatalf("empty bucket: %v", err)
	}
}

func TestOpenFetched(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	files := []masterbackup.File{
		masterbackup.NewFile(masterbackup.ClusterPath, "root", "root", 0o640, []byte("name = \"prod\"\n")),
		masterbackup.NewFile(masterbackup.NodePath, "root", "root", 0o640, []byte("server = \"prod\"\n")),
		masterbackup.NewFile(masterbackup.OperatorsPath, "root", "root", 0o640, []byte("[[operators]]\nname = 'op-1'\n")),
	}
	tarball, m, err := masterbackup.Build(masterbackup.Meta{Server: "prod", Epoch: 1, CreatedAt: time.Now(), EruVersion: "dev"}, files)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := masterbackup.Seal(tarball, id.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	side := masterbackup.NewSidecar("n", m, sealed)
	f := fetchedBundle{Name: "n", Cipher: sealed, Sidecar: &side, Older: "older"}
	b, err := openFetched(f, id.String(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := operatorInBundle(b, "op-1"); err != nil {
		t.Errorf("op-1 is in the bundle: %v", err)
	}
	if err := operatorInBundle(b, "op-2"); err == nil {
		t.Error("op-2 isn't in the bundle")
	}
	var ce *output.CLIError
	if _, err := openFetched(f, id.String(), "staging"); !errors.As(err, &ce) || ce.Code != output.CodePrecondition {
		t.Errorf("another server's bundle: %v", err)
	}
	bad := append([]byte(nil), sealed...)
	bad[len(bad)-3] ^= 1
	if _, err := openFetched(fetchedBundle{Name: "n", Cipher: bad, Sidecar: &side, Older: "older"}, id.String(), "prod"); !errors.As(err, &ce) || ce.Code != output.CodeBundleCorrupt || ce.Hint == "" {
		t.Errorf("sidecar mismatch: %v", err)
	}
	if _, err := openFetched(fetchedBundle{Name: "n", Cipher: bad}, id.String(), "prod"); !errors.As(err, &ce) || ce.Code != output.CodeBundleCorrupt {
		t.Errorf("a corrupt file without a sidecar: %v", err)
	}
}

func TestNewerThanBuild(t *testing.T) {
	if newerThanBuild("edge-abc") || newerThanBuild("1.0.0") {
		t.Error("a dev build compares versions")
	}
	if a, ok := semverParts("1.2.3"); !ok || a != [3]int{1, 2, 3} {
		t.Error("semverParts")
	}
	if _, ok := semverParts("1.2"); ok {
		t.Error("semverParts accepted 1.2")
	}
}
