package steps

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestClusterPrimaryStatus(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		valid       bool
	}{
		{"online", "online", true},
		{"online,recovery", "online", true},
		{"down", "down", true},
		{"down,recovery", "down", true},
		{"online,recovery,patroni,binaries_missing", "online", true},
		{"down,pacemaker", "down", true},
		{"", "", false},
		{"onlinex", "", false},
		{"notonline", "", false},
		{"recovery,online", "", false},
		{"online,", "", false},
		{"online,unknown", "", false},
		{"online,recoveryish", "", false},
		{"online,recovery,recovery", "", false},
		{"online,pacemaker,patroni", "", false},
		{"online,manual", "", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := clusterPrimaryStatus(tc.input)
			if (err == nil) != tc.valid || got != tc.want {
				t.Fatalf("clusterPrimaryStatus(%q) = %q, %v", tc.input, got, err)
			}
		})
	}
}

func TestParseClusterNamesStrict(t *testing.T) {
	const good = "18 app 5433 online,recovery postgres /var/lib/postgresql/18/app /var/log/postgresql/app.log\n" +
		"18 stopped 5434 down,recovery postgres /var/lib/postgresql/18/stopped /var/log/postgresql/stopped.log\n" +
		"9.6 old 5435 online postgres /var/lib/postgresql/9.6/old /var/log/postgresql/old.log\n"
	got, err := parseClusterNamesStrict(good)
	want := map[string]string{"app": "online", "stopped": "down"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
	for _, empty := range []string{"", "\n  \n", "17 other 5432 online postgres /data /log\n"} {
		got, err = parseClusterNamesStrict(empty)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty inventory %q: %v, %v", empty, got, err)
		}
	}
	for _, bad := range []string{
		"18 incomplete 5433 online postgres /data",
		"oops name 5433 online postgres /data /log",
		"18. name 5433 online postgres /data /log",
		"18 name bad online postgres /data /log",
		"18 name 0 online postgres /data /log",
		"18 name 65536 online postgres /data /log",
		"18 name 5433 online <unknown> /data /log",
		"18 name 5433 online postgres <unknown> /log",
		"18 name 5433 online postgres /data <unknown>",
		"18 name 5433 onlineish postgres /data /log",
		"17 name 5433 offline postgres /data /log",
		"18 name 5433 online,other postgres /data /log",
		"18 app 5433 online postgres /data /log",
	} {
		t.Run(bad, func(t *testing.T) {
			got, err := parseClusterNamesStrict(good + bad + "\n")
			if err == nil || got != nil {
				t.Fatalf("partial inventory returned %v, %v", got, err)
			}
		})
	}
}

func TestClusterNamesStrictProbeFailure(t *testing.T) {
	failure := errors.New("probe failed")
	probe := func(_ context.Context, name string, args ...string) (string, error) {
		if name != "pg_lsclusters" || !reflect.DeepEqual(args, []string{"-h"}) {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return "18 app 5433 online postgres /data /log", failure
	}
	got, err := clusterNamesStrict(context.Background(), probe)
	if got != nil || !errors.Is(err, failure) {
		t.Fatalf("got %v, %v", got, err)
	}
}
