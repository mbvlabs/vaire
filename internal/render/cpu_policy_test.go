package render

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestPGCPUWeightAndDropin(t *testing.T) {
	res := config.Resources{MemoryMiB: 512, CPUWeight: 73, CPUMode: "spare", CPUQuota: "150%", IOWeight: 100}
	out := string(PGServiceDropin(res))
	if !strings.Contains(out, "CPUWeight=idle") || !strings.Contains(out, "CPUQuota=150%") || res.CPUWeight != 73 {
		t.Fatal(out)
	}
	res.CPUMode = "normal"
	out = string(PGServiceDropin(res))
	if !strings.Contains(out, "CPUWeight=73") || !strings.Contains(out, "CPUQuota=150%") {
		t.Fatal(out)
	}
	res.CPUQuota = ""
	if strings.Contains(string(PGServiceDropin(res)), "CPUQuota=") {
		t.Fatal("unset quota rendered")
	}
}
