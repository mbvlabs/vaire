package cli

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestPsqlSocketIsPrivateAndRemoved(t *testing.T) {
	ln, dir, cleanup, err := listenPsqlSocket(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("proxy directory is not private: %v, %v", info, err)
	}

	var dial net.Dialer

	conn, err := dial.DialContext(t.Context(), "unix", filepath.Join(dir, ".s.PGSQL.5432"))
	if err != nil {
		t.Fatal(err)
	}

	_ = conn.Close()

	if ln.Addr().Network() != "unix" {
		t.Fatalf("unexpected listener network %q", ln.Addr().Network())
	}

	cleanup()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("proxy directory not removed: %v", err)
	}
}

func TestPsqlArgs(t *testing.T) {
	cases := []struct {
		argv []string
		ok   bool
	}{
		{nil, true},
		{[]string{"--db", "app"}, true},
		{[]string{"--db", "app", "--", "-c", "select 1"}, true},
		{[]string{"--db", "app", "select 1"}, false},
	}
	for _, c := range cases {
		cmd := &cobra.Command{Use: "psql"}
		cmd.Flags().String("db", "", "")

		if err := cmd.Flags().Parse(c.argv); err != nil {
			t.Fatal(err)
		}

		err := psqlArgs(cmd, cmd.Flags().Args())
		if (err == nil) != c.ok {
			t.Errorf("psqlArgs(%q): err = %v, want ok = %v", c.argv, err, c.ok)
		}
	}
}
