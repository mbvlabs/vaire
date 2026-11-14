package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestHandleRestoreCleanupLegacy(t *testing.T) {
	for _, mode := range []string{"present", "absent", "derived", "wrong-path", "wrong-effective-path", "wrong-cluster", "wrong-port", "symlink", "replacement", "record-change", "drop-failure", "retained", "marker", "collision", "reservation-replaced"} {
		t.Run(mode, func(t *testing.T) {
			_, root := materialisationFixture(t)
			ports := filepath.Join(materialisationAncestorBase, "ports")
			for _, dir := range []string{root, ports} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			oldPath, oldRes, oldProbe, oldDrop, oldExists := backup.RecordsPath, legacyLeftoverReservation, legacyLeftoverEffectiveSetting, restoreCleanupDrop, legacyLeftoverExists
			t.Cleanup(func() {
				backup.RecordsPath, legacyLeftoverReservation, legacyLeftoverEffectiveSetting, restoreCleanupDrop, legacyLeftoverExists = oldPath, oldRes, oldProbe, oldDrop, oldExists
			})
			backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")
			name := "app__pre_restore_20260101T010203"
			data, conf := filepath.Join(root, name), filepath.Join(materialisationConfigRoot, name)
			if err := os.Mkdir(data, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(conf, 0o755); err != nil { //nolint:gosec // registration directory fixture
				t.Fatal(err)
			}
			port := config.TemporaryPortMin
			configText := fmt.Sprintf("data_directory = '%s'\ncluster_name = '18/%s'\nport = %d\n", data, name, port)
			if mode == "wrong-path" {
				configText = fmt.Sprintf("data_directory = '%s'\ncluster_name = '18/%s'\nport = %d\n", root, name, port)
			}
			if err := os.WriteFile(filepath.Join(conf, "postgresql.conf"), []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "symlink" {
				if err := os.Rename(data, data+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(data+"-old", data); err != nil {
					t.Fatal(err)
				}
			}
			l := backup.Leftover{Database: "app", Name: name, RestoreID: "restore", Port: port, CreatedAt: "now", Stanza: "app-abcdef"}
			if mode == "derived" {
				l.Port = 0
			}
			other := backup.Leftover{Database: "other", Name: "other__pre_restore_20260101T010203", RestoreID: "other"}
			if err := backup.UpdateRecords(func(s *backup.Records) error {
				s.Leftovers = []backup.Leftover{l, other}
				if mode == "retained" {
					s.Restores["app"] = backup.Restore{ID: "unfinished", Database: "app", Attempt: &backup.MaterialisationAttempt{Target: "app__restoring"}}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if mode != "absent" {
				if err := os.WriteFile(filepath.Join(ports, name), []byte(fmt.Sprintf("%d\n", port)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "marker" {
				if err := os.WriteFile(filepath.Join(materialisationStateDir, "materialise-"+name+".json"), []byte("retained"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "collision" {
				if err := os.WriteFile(filepath.Join(ports, "another"), []byte(fmt.Sprintf("%d\n", port)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.DefaultCluster()
			cfg.Server.DataRoot = root
			cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5432, Stanza: "app-abcdef"}}
			legacyLeftoverExists = func(context.Context, string) bool { return true }
			legacyLeftoverReservation = func(ctx context.Context, name string, p int, effect func() error) error {
				return backup.CleanupCurrentLegacyReservationWithHost(ctx, ports, materialisationRecordUID, name, p, effect)
			}
			checks := 0
			legacyLeftoverEffectiveSetting = func(_ string, setting string) (string, error) {
				switch setting {
				case "data_directory":
					checks++
					if mode == "replacement" && checks == 2 {
						if err := os.Rename(data, data+"-old"); err != nil {
							return "", err
						}
						if err := os.Mkdir(data, 0o700); err != nil {
							return "", err
						}
					}
					if mode == "wrong-effective-path" {
						return root, nil
					}
					return data, nil
				case "cluster_name":
					if mode == "wrong-cluster" {
						return "18/other", nil
					}
					return "18/" + name, nil
				case "port":
					if mode == "wrong-port" {
						return "1", nil
					}
					return fmt.Sprint(port), nil
				}
				return "", errors.New("unexpected setting")
			}
			drops := 0
			restoreCleanupDrop = func(context.Context, *Env, string) error {
				drops++
				switch mode {
				case "drop-failure":
					return errors.New("drop failed")
				case "record-change":
					return backup.UpdateRecords(func(s *backup.Records) error { s.Leftovers[0].Stanza = "changed"; return nil })
				case "reservation-replaced":
					path := filepath.Join(ports, name)
					if err := os.Rename(path, path+"-old"); err != nil {
						return err
					}
					return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", port)), 0o600)
				}
				return nil
			}
			params, _ := json.Marshal(restoreCleanupParams{Database: "app", Drop: true})
			_, err := handleRestoreCleanup(context.Background(), &Env{Cluster: cfg, Req: rpc.Request{Params: params, Confirm: "app/cleanup"}})
			success := mode == "present" || mode == "absent" || mode == "derived"
			if success != (err == nil) {
				t.Fatalf("cleanup error: %v", err)
			}
			records, loadErr := backup.LoadRecords()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if success {
				if drops != 1 || len(records.Leftovers) != 1 || records.Leftovers[0] != other {
					t.Fatalf("unexpected retirement: %+v drops=%d", records.Leftovers, drops)
				}
				if _, err := os.Lstat(filepath.Join(ports, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("reservation remains: %v", err)
				}
			} else {
				if len(records.Leftovers) != 2 {
					t.Fatalf("failure lost record: %+v", records.Leftovers)
				}
				if _, err := os.Lstat(filepath.Join(ports, name)); err != nil {
					t.Fatalf("failure lost reservation: %v", err)
				}
			}
		})
	}
}
