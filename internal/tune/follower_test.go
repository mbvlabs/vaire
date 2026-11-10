package tune

import "testing"

func TestWalLevelIsReplica(t *testing.T) {
	res := Calculate(Input{RAMMiB: 1024, VCPU: 2, Clusters: 1, DataDiskGiB: 40, Profile: ProfileMixed, PGMajor: 18})
	got := map[string]string{}
	for _, s := range res.Settings {
		got[s.Name] = s.Value
	}
	if got["wal_level"] != "replica" {
		t.Fatalf("wal_level %q, want replica (Phase 6 physical followers)", got["wal_level"])
	}
	if got["max_slot_wal_keep_size"] != "4GB" {
		t.Fatalf("max_slot_wal_keep_size %q, want 4GB for a 40 GiB share", got["max_slot_wal_keep_size"])
	}
}

func TestSlotWALKeepClamps(t *testing.T) {
	for disk, want := range map[int]float64{0: 2, 10: 2, 100: 10, 640: 64, 4000: 64} {
		if got := SlotWALKeepGiB(disk); got != want {
			t.Errorf("SlotWALKeepGiB(%d) = %v, want %v", disk, got, want)
		}
	}
}
