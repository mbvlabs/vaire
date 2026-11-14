package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStatusWatchCPU(t *testing.T) {
	sample := func(usage uint64, timeNS int64, generation string) statusData {
		return statusData{Databases: []statusDB{{Name: "f", CPU: &statusCPU{UsageUsec: &usage, SampleMonotonicNS: &timeNS, SampleGeneration: generation}}}}
	}
	first, second := sample(100, 1000000000, "same"), sample(500100, 2000000000, "same")
	statusWatchCPU(first, &second)
	if second.Databases[0].CPU.UsagePercent == nil || *second.Databases[0].CPU.UsagePercent != 50 {
		t.Fatal(second.Databases[0].CPU)
	}
	for _, next := range []statusData{sample(99, 2000000000, "same"), sample(101, 1000000000, "same"), sample(101, 2000000000, "new"), sample(101, 2000000000, "")} {
		statusWatchCPU(first, &next)
		if next.Databases[0].CPU.UsagePercent != nil {
			t.Fatal(next.Databases[0].CPU)
		}
	}
	missing := sample(200, 2000000000, "same")
	missing.Databases[0].CPU.UsageUsec = nil
	statusWatchCPU(first, &missing)
	if missing.Databases[0].CPU.UsagePercent != nil {
		t.Fatal("missing usage accepted")
	}
}

func TestStatusCPUJSONAndHuman(t *testing.T) {
	var d statusData
	raw := `{"databases":[{"name":"f","state":"running","follows":"app","cpu":{"requested":{"mode":"spare","quota":"150%"},"effective_known":true,"effective":{"idle":true,"unlimited":true,"quota_period_usec":100000,"weight":1},"usage_usec":20},"data_free_bytes":0}]}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	out := statusMessage(d, nil)
	for _, s := range []string{"CPU spare", "effective quota unlimited", "cumulative usage 20 usec", "0 bytes free"} {
		if !strings.Contains(out, s) {
			t.Fatalf("missing %q in %s", s, out)
		}
	}
	encoded, err := json.Marshal(d)
	if err != nil || strings.Contains(string(encoded), `"quota_usec":0`) {
		t.Fatalf("%s %v", encoded, err)
	}
}
