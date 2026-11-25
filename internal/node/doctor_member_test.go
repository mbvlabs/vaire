package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/doctor"
	"github.com/mbvlabs/vaire/internal/rpc"
)

type memberDoctorPG struct{}

func (memberDoctorPG) Query(context.Context, string, string, map[string]string) ([]map[string]string, error) {
	return nil, errors.New("secret connection error")
}

func (memberDoctorPG) Exec(context.Context, string, string, map[string]string) error {
	return nil
}

func TestMemberDoctorBasicAvailability(t *testing.T) {
	oldRunning, oldPG := clusterRunning, snapshotPG
	clusterRunning = func(config.ClusterSpec) bool { return true }
	snapshotPG = func(config.ClusterSpec) sqlRunner { return memberDoctorPG{} }

	t.Cleanup(func() { clusterRunning, snapshotPG = oldRunning, oldPG })

	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{{Name: "private_database"}}
	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{Role: config.AccessRoleMember}}

	for _, tc := range []struct {
		params string
		status doctor.Status
		count  int
	}{
		{`{}`, doctor.Fail, 1},
		{`{"only":"postgres"}`, doctor.Fail, 1},
	} {
		env.Req.Params = json.RawMessage(tc.params)

		res, err := handleDoctorRun(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}

		data := mustAs[doctorData](t, res.Data)
		if !data.Basic || len(data.Results) != tc.count || (tc.count > 0 && data.Results[0].Status != tc.status) {
			t.Fatalf("%s: unexpected response: %+v", tc.params, data)
		}

		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}

		for _, secret := range []string{"private_database", "secret connection error", cfg.Server.DataRoot} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("%s: leaked %q in %s", tc.params, secret, raw)
			}
		}
	}
}

func TestMemberDoctorDeniesDetailedChecks(t *testing.T) {
	for _, p := range []doctorParams{{DB: "private_database"}, {Only: doctor.CatHost}} {
		if _, err := handleMemberDoctor(context.Background(), config.Cluster{}, p); err == nil {
			t.Fatalf("detailed check was allowed: %+v", p)
		}
	}
}

func TestMemberDoctorAvailabilityStates(t *testing.T) {
	for _, tc := range []struct {
		clusters []clusterSnapshot
		status   doctor.Status
	}{
		{nil, doctor.Skip},
		{[]clusterSnapshot{{State: "running"}}, doctor.Pass},
		{[]clusterSnapshot{{State: "running"}, {State: "stopped"}}, doctor.Fail},
		{[]clusterSnapshot{{State: "unreachable", Error: "sensitive"}}, doctor.Fail},
	} {
		results := memberDoctorResults(tc.clusters)
		if len(results) != 1 || results[0].Status != tc.status {
			t.Fatalf("unexpected results: %+v", results)
		}
	}
}

func TestMemberDoctorHidesMaterialisationDetails(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	env := &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{Role: config.AccessRoleMember}}

	_, err := handleDoctorRun(context.Background(), env)
	if err == nil {
		t.Fatal("expected a refusal")
	}

	raw, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}

	for _, secret := range []string{o.Target, o.Job, root} {
		if secret != "" && strings.Contains(string(raw), secret) {
			t.Fatalf("refusal leaked %q: %s", secret, raw)
		}
	}
}
