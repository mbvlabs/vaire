package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMemberTuneDataOmitsServer(t *testing.T) {
	var data tuneShowData
	if err := json.Unmarshal([]byte(`{"database":"mine","settings":[{"name":"work_mem","value":"4MB"}]}`), &data); err != nil {
		t.Fatal(err)
	}

	if data.Server != nil || data.Database != "mine" || len(data.Settings) != 1 {
		t.Fatalf("unexpected tune data: %+v", data)
	}

	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), `"server"`) {
		t.Fatalf("fabricated server sheet: %s", raw)
	}
}
