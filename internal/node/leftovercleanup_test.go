package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestHandleRestoreCleanupLegacy(t *testing.T) {
	for _, mode := range []string{"present", "absent", "derived", "wrong-path", "wrong-effective-path", "wrong-cluster", "wrong-port", "symlink", "replacement", "record-change", "drop-failure", "retained", "marker", "collision", "reservation-replaced"} {
		t.Run(mode, func(t *testing.T) {
			runHandleRestoreCleanupLegacy(t, mode)
		})
	}
}

// legacyCleanupState holds the legacy leftover layout and observations the
// cleanup assertions need after handleRestoreCleanup returns.
type legacyCleanupState struct {
	mode   string
	root   string
	ports  string
	name   string
	data   string
	conf   string
	port   int
	record backup.Leftover
	other  backup.Leftover
	cfg    config.Cluster
	checks int
	drops  int
}

func runHandleRestoreCleanupLegacy(t *testing.T, mode string) {
	t.Helper()
	s := newLegacyCleanupState(t, mode)
	stubLegacyCleanup(t, s)
	params, _ := json.Marshal(restoreCleanupParams{Database: "app", Drop: true})
	_, err := handleRestoreCleanup(context.Background(), &Env{Cluster: s.cfg, Req: rpc.Request{Params: params, Confirm: "app/cleanup"}})
	assertLegacyCleanup(t, s, err)
}

func newLegacyCleanupState(t *testing.T, mode string) *legacyCleanupState {
	t.Helper()
	_, root := materialisationFixture(t)
	s := &legacyCleanupState{
		mode:  mode,
		root:  root,
		ports: filepath.Join(materialisationAncestorBase, "ports"),
		name:  "app__pre_restore_20260101T010203",
		port:  config.TemporaryPortMin,
		other: backup.Leftover{Database: "other", Name: "other__pre_restore_20260101T010203", RestoreID: "other"},
	}
	s.data, s.conf = filepath.Join(root, s.name), filepath.Join(materialisationConfigRoot, s.name)
	for _, dir := range []string{root, s.ports} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldPath, oldRes, oldProbe, oldDrop, oldExists := backup.RecordsPath, legacyLeftoverReservation, legacyLeftoverEffectiveSetting, restoreCleanupDrop, legacyLeftoverExists
	t.Cleanup(func() {
		backup.RecordsPath, legacyLeftoverReservation, legacyLeftoverEffectiveSetting, restoreCleanupDrop, legacyLeftoverExists = oldPath, oldRes, oldProbe, oldDrop, oldExists
	})
	backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")
	if err := os.Mkdir(s.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.conf, 0o755); err != nil {
		t.Fatal(err)
	}
	mustNoErr(t, os.WriteFile(filepath.Join(s.conf, "postgresql.conf"), []byte(s.configText()), 0o600))
	if mode == "symlink" {
		mustNoErr(t, os.Rename(s.data, s.data+"-old"))
		mustNoErr(t, os.Symlink(s.data+"-old", s.data))
	}
	s.record = backup.Leftover{Database: "app", Name: s.name, RestoreID: "restore", Port: s.port, CreatedAt: "now", Stanza: "app-abcdef"}
	if mode == "derived" {
		s.record.Port = 0
	}
	mustNoErr(t, backup.UpdateRecords(func(r *backup.Records) error {
		r.Leftovers = []backup.Leftover{s.record, s.other}
		if mode == "retained" {
			r.Restores["app"] = backup.Restore{ID: "unfinished", Database: "app", Attempt: &backup.MaterialisationAttempt{Target: "app__restoring"}}
		}
		return nil
	}))
	if mode != "absent" {
		mustNoErr(t, os.WriteFile(filepath.Join(s.ports, s.name), []byte(fmt.Sprintf("%d\n", s.port)), 0o600))
	}
	if mode == "marker" {
		mustNoErr(t, os.WriteFile(filepath.Join(materialisationStateDir, "materialise-"+s.name+".json"), []byte("retained"), 0o600))
	}
	if mode == "collision" {
		mustNoErr(t, os.WriteFile(filepath.Join(s.ports, "another"), []byte(fmt.Sprintf("%d\n", s.port)), 0o600))
	}
	s.cfg = config.DefaultCluster()
	s.cfg.Server.DataRoot = root
	s.cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5432, Stanza: "app-abcdef"}}
	return s
}

func (s *legacyCleanupState) configText() string {
	dir := s.data
	if s.mode == "wrong-path" {
		dir = s.root
	}
	return fmt.Sprintf("data_directory = '%s'\ncluster_name = '18/%s'\nport = %d\n", dir, s.name, s.port)
}

// stubLegacyCleanup installs the legacy probes. The data_directory probe
// replaces the data directory on its second read so cleanup must re-verify the
// path it already opened.
func stubLegacyCleanup(t *testing.T, s *legacyCleanupState) {
	t.Helper()
	legacyLeftoverExists = func(context.Context, string) bool { return true }
	legacyLeftoverReservation = func(ctx context.Context, name string, p int, effect func() error) error {
		return backup.CleanupCurrentLegacyReservationWithHost(ctx, s.ports, materialisationRecordUID, name, p, effect)
	}
	stubLegacySettingProbe(s)
	stubLegacyDrop(s)
}

func stubLegacySettingProbe(s *legacyCleanupState) {
	legacyLeftoverEffectiveSetting = func(_ string, setting string) (string, error) {
		switch setting {
		case "data_directory":
			return s.legacyDataDir()
		case "cluster_name":
			return s.legacyClusterName()
		case "port":
			return s.legacyPort()
		}
		return "", errors.New("unexpected setting")
	}
}

// legacyDataDir replaces the data directory on the second read so cleanup can
// only proceed if it verifies the path it opened rather than the one it was
// told about.
func (s *legacyCleanupState) legacyDataDir() (string, error) {
	s.checks++
	if s.mode == "replacement" && s.checks == 2 {
		if err := os.Rename(s.data, s.data+"-old"); err != nil {
			return "", err
		}
		if err := os.Mkdir(s.data, 0o700); err != nil {
			return "", err
		}
	}
	if s.mode == "wrong-effective-path" {
		return s.root, nil
	}
	return s.data, nil
}

func (s *legacyCleanupState) legacyClusterName() (string, error) {
	if s.mode == "wrong-cluster" {
		return "18/other", nil
	}
	return "18/" + s.name, nil
}

func (s *legacyCleanupState) legacyPort() (string, error) {
	if s.mode == "wrong-port" {
		return "1", nil
	}
	return fmt.Sprint(s.port), nil
}

func stubLegacyDrop(s *legacyCleanupState) {
	restoreCleanupDrop = func(context.Context, *Env, string) error {
		s.drops++
		switch s.mode {
		case "drop-failure":
			return errors.New("drop failed")
		case "record-change":
			return backup.UpdateRecords(func(r *backup.Records) error { r.Leftovers[0].Stanza = "changed"; return nil })
		case "reservation-replaced":
			path := filepath.Join(s.ports, s.name)
			if err := os.Rename(path, path+"-old"); err != nil {
				return err
			}
			return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", s.port)), 0o600)
		}
		return nil
	}
}

func assertLegacyCleanup(t *testing.T, s *legacyCleanupState, err error) {
	t.Helper()
	success := s.mode == "present" || s.mode == "absent" || s.mode == "derived"
	rejectf(t, success != (err == nil), "cleanup error: %v", err)
	records, loadErr := backup.LoadRecords()
	reject(t, loadErr != nil, loadErr)
	if success {
		if s.drops != 1 || len(records.Leftovers) != 1 || records.Leftovers[0] != s.other {
			t.Fatalf("unexpected retirement: %+v drops=%d", records.Leftovers, s.drops)
		}
		if _, err := os.Lstat(filepath.Join(s.ports, s.name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reservation remains: %v", err)
		}
		return
	}
	rejectf(t, len(records.Leftovers) != 2, "failure lost record: %+v", records.Leftovers)
	if _, err := os.Lstat(filepath.Join(s.ports, s.name)); err != nil {
		t.Fatalf("failure lost reservation: %v", err)
	}
}

// The effective setting is pg_conftool's short output; the labelled default
// never equals the bare value.
func TestLegacyLeftoverEffectiveSettingUsesShortConftoolOutput(t *testing.T) {
	const name = "app__pre_restore_20260101T010203"
	args := fakePgConftoolSettings(t, map[string]fakePgConftoolSetting{
		"port": {value: "5482"},
	})
	got, err := legacyLeftoverEffectiveSetting(name, "port")
	if err != nil || got != "5482" {
		t.Fatalf("effective setting: got %q, err = %v", got, err)
	}
	if got, want := args(), "-s 18 "+name+" show port"; got != want {
		t.Fatalf("pg_conftool args = %q, want %q", got, want)
	}
}

func TestLegacyLeftoverRegistrationUsesShortConftoolOutput(t *testing.T) {
	root, name, port, settings := legacyLeftoverSettingFixture(t)
	fakePgConftoolSettings(t, settings)
	if err := legacyLeftoverRegistration(name, "app", root, 0, port); err != nil {
		t.Fatalf("registration with exact effective settings: %v", err)
	}
}

func TestLegacyLeftoverRegistrationRefusesUnverifiableSettings(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		status        int
	}{
		{"mismatched data directory", "data_directory", 0},
		{"failed cluster name", "cluster_name", 1},
		{"failed port", "port", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, name, port, settings := legacyLeftoverSettingFixture(t)
			s := settings[tc.setting]
			if tc.status == 0 {
				s.value = "wrong"
			}
			s.status = tc.status
			settings[tc.setting] = s
			fakePgConftoolSettings(t, settings)
			if err := legacyLeftoverRegistration(name, "app", root, 0, port); err == nil ||
				err.Error() != "effective legacy "+tc.setting+" is unverifiable" {
				t.Fatalf("unverifiable %s: err = %v", tc.setting, err)
			}
		})
	}
}

// The direct registration guard runs before postgresql-common is asked
// anything.
func TestLegacyLeftoverRegistrationVerifiesDirectPortBeforeConftool(t *testing.T) {
	root, name, port := t.TempDir(), "app__pre_restore_20260101T010203", config.TemporaryPortMin
	old := legacyLeftoverPort
	legacyLeftoverPort = func(_, _, _ string) (int, error) { return port + 1, nil }
	t.Cleanup(func() { legacyLeftoverPort = old })
	args := fakePgConftoolSettings(t, map[string]fakePgConftoolSetting{
		"data_directory": {value: filepath.Join(root, name)},
		"cluster_name":   {value: "18/" + name},
		"port":           {value: strconv.Itoa(port)},
	})
	if err := legacyLeftoverRegistration(name, "app", root, 0, port); err == nil ||
		err.Error() != "direct legacy registration is unverifiable" {
		t.Fatalf("mismatched direct port: err = %v", err)
	}
	if got := args(); got != "" {
		t.Fatalf("pg_conftool ran before the direct registration check: %q", got)
	}
}

func legacyLeftoverSettingFixture(t *testing.T) (string, string, int, map[string]fakePgConftoolSetting) {
	t.Helper()
	root, name, port := t.TempDir(), "app__pre_restore_20260101T010203", config.TemporaryPortMin
	settings := map[string]fakePgConftoolSetting{
		"data_directory": {value: filepath.Join(root, name)},
		"cluster_name":   {value: "18/" + name},
		"port":           {value: strconv.Itoa(port)},
	}
	old := legacyLeftoverPort
	legacyLeftoverPort = func(_, _, _ string) (int, error) { return port, nil }
	t.Cleanup(func() { legacyLeftoverPort = old })
	return root, name, port, settings
}
