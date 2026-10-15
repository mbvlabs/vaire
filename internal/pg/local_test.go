package pg

import (
	"strings"
	"testing"
)

func TestLocalCommand(t *testing.T) {
	name, args, err := Local{Host: "/var/run/postgresql", Port: 5433}.command("app", map[string]string{"v": "1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if name != "runuser" || strings.Join(args, " ") != "-u postgres -- psql -X -q -v ON_ERROR_STOP=1 -h /var/run/postgresql -p 5433 -v v=1 --csv -d app -f -" {
		t.Errorf("%s %v", name, args)
	}
	name, args, err = Local{Host: "/var/run/postgresql", Port: 5433, User: "eru_admin", AsRoot: true}.command("app", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if name != "psql" || strings.Join(args, " ") != "-X -q -v ON_ERROR_STOP=1 -h /var/run/postgresql -p 5433 -U eru_admin -d app -f -" {
		t.Errorf("as root: %s %v", name, args)
	}
	if _, _, err := (Local{}).command("app", map[string]string{"Bad-Name": "x"}, false); err == nil {
		t.Error("a bad variable name was accepted")
	}
}
