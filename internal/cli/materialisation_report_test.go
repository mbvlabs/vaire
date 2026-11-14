package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIncompleteRowsHumanAndJSON(t *testing.T) {
	ready, recovery, paused := false, true, true
	report := &materialisationReport{Job: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Phase: "failed",
		Reason: "recovery_parameter_mismatch", PhysicalState: "online", InRecovery: &recovery, ReplayPaused: &paused}
	db := databaseInfo{Name: "target", InConfig: true, State: "incomplete", ApplicationReady: &ready, Materialisation: report}
	status := statusDB{Name: "target", State: "incomplete", ApplicationReady: &ready, Materialisation: report}
	for _, item := range []struct {
		value any
		human string
	}{
		{db, strings.Join(dbListLines([]databaseInfo{db}), "\n")},
		{status, statusMessage(statusData{Databases: []statusDB{status}}, nil)},
	} {
		raw, err := json.Marshal(item.value)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"state":"incomplete"`, `"application_ready":false`, `"physical_state":"online"`, `"replay_paused":true`} {
			if !strings.Contains(string(raw), want) {
				t.Fatalf("JSON missing %s: %s", want, raw)
			}
		}
		if strings.Contains(string(raw), `"connections"`) || strings.Contains(string(raw), `"exists"`) {
			t.Fatalf("invented application data: %s", raw)
		}
		for _, want := range []string{"incomplete", "application unavailable", "physical online", "replay paused true"} {
			if !strings.Contains(item.human, want) {
				t.Fatalf("human missing %q: %s", want, item.human)
			}
		}
	}
	data := statusData{Databases: []statusDB{status}, Budget: tuneServer{
		Clusters: []tuneServerCluster{{Name: "target"}},
	}}
	raw, err := json.Marshal(data)
	if err != nil || strings.Contains(string(raw), `"running"`) {
		t.Fatalf("incomplete budget presented as stopped: %s %v", raw, err)
	}
}

func TestStatusBudgetRunningPresenceIndependentOfDatabaseFilter(t *testing.T) {
	for _, tc := range []struct {
		name      string
		databases string
	}{
		{"another database selected", `[{"name":"other","state":"running"}]`},
		{"one incomplete selected", `[{"name":"partial_a","state":"incomplete"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"budget":{"clusters":[` +
				`{"name":"partial_a","memory_mib":128},` +
				`{"name":"partial_b","memory_mib":256},` +
				`{"name":"stopped","running":false},` +
				`{"name":"running","running":true}` +
				`]},"databases":` + tc.databases + `}`
			var data statusData
			if err := json.Unmarshal([]byte(payload), &data); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Budget struct {
					Clusters []map[string]json.RawMessage `json:"clusters"`
				} `json:"budget"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Budget.Clusters) != 4 {
				t.Fatalf("budget allocations lost: %s", raw)
			}
			for i, expected := range []string{"", "", "false", "true"} {
				value, present := got.Budget.Clusters[i]["running"]
				if present != (expected != "") || present && string(value) != expected {
					t.Errorf("cluster %d running = %s, present %v; want %s", i, value, present, expected)
				}
			}
			sheet := strings.Join(sheetLines("test", data.Budget), "\n")
			for _, want := range []string{"partial_a", "partial_b", "unknown", "stopped", "running"} {
				if !strings.Contains(sheet, want) {
					t.Errorf("budget rendering missing %q: %s", want, sheet)
				}
			}
		})
	}
}
