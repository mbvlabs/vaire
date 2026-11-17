package node

import (
	"context"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// restoreTargetDriver answers only the two reads a time target makes.
type restoreTargetDriver struct {
	backup.Driver
	info    backup.Info
	archive backup.ArchiveStatus
}

func (d restoreTargetDriver) Info(context.Context, string) (backup.Info, error) {
	return d.info, nil
}

func (d restoreTargetDriver) ArchiveStatus(context.Context) (backup.ArchiveStatus, error) {
	return d.archive, nil
}

func targetWindowDriver() restoreTargetDriver {
	return restoreTargetDriver{
		info: backup.Info{Sets: []backup.Set{{
			Label: "F1", Type: backup.TypeFull, Start: "2026-11-17T17:55:00Z", Stop: "2026-11-17T18:00:00Z",
		}}},
		archive: backup.ArchiveStatus{LastArchivedTime: "2026-11-17T19:00:00Z"},
	}
}

// The target the node records is the exact parsed instant, not a whole
// second.
func TestRestoreTimeTargetKeepsFraction(t *testing.T) {
	spec := config.ClusterSpec{Name: "app", Stanza: "app-stanza"}
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"microseconds", "2026-11-17T18:44:53.983364Z", "2026-11-17T18:44:53.983364Z"},
		{"offset", "2026-11-17T19:44:53.983364+01:00", "2026-11-17T18:44:53.983364Z"},
		{"nanoseconds", "2026-11-17T18:44:53.983364123Z", "2026-11-17T18:44:53.983364123Z"},
		{"whole second", "2026-11-17T18:44:53Z", "2026-11-17T18:44:53Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := restoreTimeTarget(context.Background(), targetWindowDriver(), spec, restoreParams{
				Database: "app", Kind: backup.TargetTime, Time: c.value,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("target %q, want %q", got, c.want)
			}
		})
	}
}

func TestRestoreTimeTargetRefusesInvalidTime(t *testing.T) {
	_, err := restoreTimeTarget(context.Background(), targetWindowDriver(), config.ClusterSpec{Name: "app"}, restoreParams{
		Database: "app", Kind: backup.TargetTime, Time: "2026-11-17 18:44:53",
	})
	if err == nil {
		t.Fatal("invalid time was accepted")
	}
}

// The window check uses the parsed instant, so a target just after the last
// archived WAL is refused even though its whole second is inside.
func TestRestoreWindowKeepsFractionalInstant(t *testing.T) {
	w := Window{From: "2026-11-17T18:00:01Z", To: "2026-11-17T19:00:00Z"}
	at, err := time.Parse(time.RFC3339Nano, "2026-11-17T19:00:00.000001Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := inWindow(w, at); err == nil {
		t.Fatal("a target after the window's end was accepted")
	}
}

// startDrill records the exact target in the drill record and hands the same
// instant to the restore.
func TestDrillTargetKeepsFraction(t *testing.T) {
	old := drillReadInfo
	drillReadInfo = func(context.Context, config.ClusterSpec, string, engine.Reporter) (backup.Info, error) {
		return backup.Info{Sets: []backup.Set{{Label: "F1", Type: backup.TypeFull}}}, nil
	}
	t.Cleanup(func() { drillReadInfo = old })

	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"fraction", "2026-11-17T18:44:53.983364Z", "2026-11-17T18:44:53.983364Z"},
		{"offset", "2026-11-17T19:44:53.983364+01:00", "2026-11-17T18:44:53.983364Z"},
		{"whole second", "2026-11-17T18:44:53Z", "2026-11-17T18:44:53Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			drill := backup.Drill{Database: "app"}
			opts, _, latest, err := startDrill(context.Background(), &jobEnv{Job: "restore-drill"}, drillParams{Time: c.value},
				config.ClusterSpec{Name: "app", Stanza: "app-stanza"}, "app", backup.Records{}, &drill, func() {})
			if err != nil {
				t.Fatal(err)
			}
			if latest.Label != "F1" || opts.Kind != backup.TargetTime {
				t.Fatalf("drill options %+v, latest %+v", opts, latest)
			}
			want, err := time.Parse(time.RFC3339Nano, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if !opts.Time.Equal(want) {
				t.Errorf("drill restore target %s, want %s", opts.Time, want)
			}
			if drill.Target != c.want {
				t.Errorf("drill record target %q, want %q", drill.Target, c.want)
			}
		})
	}
}

func TestDrillTargetRefusesInvalidTime(t *testing.T) {
	_, _, _, err := startDrill(context.Background(), &jobEnv{Job: "restore-drill"}, drillParams{Time: "2026-11-17 18:44:53"},
		config.ClusterSpec{Name: "app", Stanza: "app-stanza"}, "app", backup.Records{}, &backup.Drill{}, func() {})
	if err == nil {
		t.Fatal("invalid drill time was accepted")
	}
}

// A ready restore's record round-trips its target into the options promote
// rebuilds, so the restart at the cut-over keeps the fraction.
func TestRestoreRecordTargetKeepsFraction(t *testing.T) {
	want, err := time.Parse(time.RFC3339Nano, "2026-11-17T18:44:53.983364Z")
	if err != nil {
		t.Fatal(err)
	}
	opts := restoreOptionsFromRecord(backup.Restore{
		Kind: backup.TargetTime, Target: "2026-11-17T18:44:53.983364Z",
	})
	if !opts.Time.Equal(want) {
		t.Errorf("promote target %s, want %s", opts.Time, want)
	}
}
