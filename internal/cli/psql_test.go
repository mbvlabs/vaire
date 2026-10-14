package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

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
