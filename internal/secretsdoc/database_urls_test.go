package secretsdoc

import (
	"reflect"
	"testing"

	"github.com/mbvlabs/vaire/internal/workspaces"
)

func TestDatabaseURLs(t *testing.T) {
	roles := map[string]workspaces.Role{
		"app":     {User: "app", Password: "app-password"},
		"migrate": {User: "migrate", Password: "migrate-password"},
	}

	for _, tc := range []struct{ mode, query string }{
		{"self-signed", "sslmode=require"},
		{"acme", "sslmode=verify-full&sslrootcert=system"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			want := map[string]string{
				"app":     "postgres://app:app-password@db.example:6432/mydb?" + tc.query,
				"migrate": "postgres://migrate:migrate-password@db.example:5432/mydb?" + tc.query,
			}
			if got := DatabaseURLs("db.example", tc.mode, "mydb", roles); !reflect.DeepEqual(got, want) {
				t.Errorf("DatabaseURLs = %v, want %v", got, want)
			}
		})
	}
}
