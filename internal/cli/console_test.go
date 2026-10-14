package cli

import "testing"

func TestConsolePrompt(t *testing.T) {
	cases := []struct{ host, ctx, want string }{
		{"db1.example.com", "prod", "PROMPT1=%S%Ndb1%/%R%# "},
		{"", "prod", "PROMPT1=%S%Nprod%/%R%# "},
		{"we%ird.example.com", "prod", "PROMPT1=%S%Nweird%/%R%# "},
	}
	for _, c := range cases {
		if got := consolePrompt(c.host, c.ctx); got != c.want {
			t.Errorf("consolePrompt(%q, %q) = %q, want %q", c.host, c.ctx, got, c.want)
		}
	}
}
