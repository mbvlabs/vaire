package cli

import (
	"context"
	"os"
	"sort"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/top"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// topRefresh is how often vaire top reads every database again.
const topRefresh = 2 * time.Second

// topConn is one database's connection through the session pooler.
type topConn struct {
	db         string
	conn       *pg.Remote
	statements bool
	err        string
}

// topCollector holds vaire top's connections: one per database through the
// session pooler as vaire_admin, read-only, plus both poolers' admin consoles
// (Phase 4 D9).
type topCollector struct {
	conns []*topConn
	pools map[string]*pg.Remote
	prev  map[string]top.Sample
	// includeVaire shows vaire's own sessions (its roles, or an application_name
	// beginning with "vaire"), left out by default.
	includeVaire bool
}

func openTop(ctx context.Context, cmd *cobra.Command, s *session, p *progress, dbs []string) (*topCollector, error) {
	user, password, err := adminCredentials(s.ctx)
	if err != nil {
		return nil, err
	}
	c := &topCollector{pools: map[string]*pg.Remote{}}
	for _, db := range dbs {
		at := databaseEndpoint(cmd, s, p, db)
		tc := &topConn{db: db}
		conn, err := pg.Connect(ctx, s.dialer, pg.Params{Host: at.WGAddress, Port: pgPort, ServerName: at.Hostname, TLSMode: s.ctx.TLSMode,
			User: user, Password: password, Database: db, ApplicationName: "vaire top"})
		if err != nil {
			tc.err = err.Error()
		} else {
			tc.conn = conn
			_, _ = conn.Exec(ctx, "SET default_transaction_read_only = on")
			if res, err := conn.Query(ctx, top.HasStatementsSQL); err == nil {
				rows := res.Maps()
				tc.statements = len(rows) == 1 && rows[0]["n"] != "0"
			}
		}
		c.conns = append(c.conns, tc)
	}
	master := databaseEndpoint(cmd, s, p, "")
	for name, port := range map[string]int{"transaction": 6432, "session": pgPort} {
		conn, err := pg.Connect(ctx, s.dialer, pg.Params{Host: master.WGAddress, Port: port, ServerName: master.Hostname, TLSMode: s.ctx.TLSMode,
			User: user, Password: password, Database: "pgbouncer", SimpleProtocol: true})
		if err == nil {
			c.pools[name] = conn
		}
	}
	return c, nil
}

func (c *topCollector) close(ctx context.Context) {
	for _, tc := range c.conns {
		tc.conn.Close(ctx)
	}
	for _, conn := range c.pools {
		conn.Close(ctx)
	}
}

// sample reads every database once and builds the snapshot against the
// previous round.
func (c *topCollector) sample(ctx context.Context, filter string) top.Snapshot {
	cur := map[string]top.Sample{}
	for _, tc := range c.conns {
		s := top.Sample{DB: tc.db, At: time.Now()}
		if tc.conn == nil {
			s.Error = tc.err
			cur[tc.db] = s
			continue
		}
		if res, err := tc.conn.Query(ctx, top.DatabaseSQL); err == nil && len(res.Maps()) == 1 {
			s.Counters = top.ParseCounters(res.Maps()[0])
		} else if err != nil {
			s.Error = err.Error()
		}
		if res, err := tc.conn.Query(ctx, top.ActivitySQL); err == nil {
			s.Sessions = top.ParseSessions(tc.db, res.Maps())
			if !c.includeVaire {
				s.Sessions = withoutVaireSessions(s.Sessions)
			}
		}
		if tc.statements {
			if res, err := tc.conn.Query(ctx, top.StatementsSQL(c.includeVaire)); err == nil {
				s.Statements = top.ParseStatements(res.Maps())
			}
		}
		cur[tc.db] = s
	}
	var pools []top.Pool
	for _, name := range []string{"transaction", "session"} {
		if conn, ok := c.pools[name]; ok {
			if res, err := conn.Query(ctx, "SHOW POOLS"); err == nil {
				pools = append(pools, top.ParsePools(name, res.Maps())...)
			}
		}
	}
	snap := top.Build(c.prev, cur, pools, filter, time.Now())
	c.prev = cur
	return snap
}

func runTop(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx := commandContext(cmd)
	dbs := topDatabases(cmd, s, p)
	filter := stringFlag(cmd, "db")
	if filter != "" {
		found := false
		for _, db := range dbs {
			found = found || db == filter
		}
		if !found {
			dbs = append(dbs, filter)
		}
	}
	if len(dbs) == 0 {
		return output.Success(cmd, output.Result{Data: top.Snapshot{At: time.Now()}, Message: "no databases yet (vaire db create <name>)", Warnings: s.warnings})
	}
	stopWait := p.wait("connecting to " + strconv.Itoa(len(dbs)) + " databases")
	c, err := openTop(ctx, cmd, s, p, dbs)
	stopWait()
	if err != nil {
		return err
	}
	c.includeVaire = flagTrue(cmd, "all-sessions")
	defer c.close(context.Background())
	interactive := !flagTrue(cmd, "once") && !MustNotPrompt(cmd) && term.IsTerminal(int(os.Stdout.Fd())) //nolint:gosec // G115: a file descriptor fits in int.
	if !interactive {
		// Two rounds a refresh apart, so the snapshot has rates.
		c.sample(ctx, filter)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(topRefresh):
		}
		snap := c.sample(ctx, filter)
		msg := top.Render(snap, filter, "time", 0, 1000)
		if !c.includeVaire {
			msg += "\ninternal/administrative roles (including postgres and maintenance roles) and sessions whose application_name begins with \"vaire\" are hidden; --all-sessions includes them"
		}
		return output.Success(cmd, output.Result{Data: snap, Message: msg, Warnings: s.warnings})
	}
	m := &topModel{ctx: ctx, c: c, dbs: append([]string{""}, dbs...), sortKey: "time", width: 120, height: 40}
	for i, db := range m.dbs {
		if db == filter {
			m.filter = i
		}
	}
	m.snap = c.sample(ctx, filter)
	_, err = tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}

// withoutVaireSessions leaves out vaire's own sessions (its roles, or an
// application_name beginning with "vaire"), so the application's sessions
// stand out. --all-sessions keeps them.
func withoutVaireSessions(sessions []top.Session) []top.Session {
	out := make([]top.Session, 0, len(sessions))
	for _, s := range sessions {
		if pg.IsVaireSession(s.Role, s.App) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// topDatabases are the databases in the context's placement cache, refreshed
// from the master when it is empty.
func topDatabases(cmd *cobra.Command, s *session, p *progress) []string {
	if len(s.ctx.Databases) == 0 {
		s.warnings = append(s.warnings, syncContextFromNode(cmd, s, p)...)
	}
	out := make([]string, 0, len(s.ctx.Databases))
	for db := range s.ctx.Databases {
		out = append(out, db)
	}
	sort.Strings(out)
	return out
}

type topTick struct{}

type topSnap struct{ snap top.Snapshot }

// topModel is vaire top's terminal program.
type topModel struct {
	ctx     context.Context
	c       *topCollector
	snap    top.Snapshot
	dbs     []string // "" is every database
	filter  int
	sortKey string
	width   int
	height  int
}

func (m *topModel) Init() tea.Cmd {
	return tea.Tick(topRefresh, func(time.Time) tea.Msg { return topTick{} })
}

func (m *topModel) refresh() tea.Cmd {
	filter := m.dbs[m.filter]
	return func() tea.Msg { return topSnap{m.c.sample(m.ctx, filter)} }
}

func (m *topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "d":
			m.filter = (m.filter + 1) % len(m.dbs)
		case "s":
			m.sortKey = map[string]string{"time": "state", "state": "db", "db": "time"}[m.sortKey]
		}
	case topTick:
		return m, m.refresh()
	case topSnap:
		m.snap = msg.snap
		return m, tea.Tick(topRefresh, func(time.Time) tea.Msg { return topTick{} })
	}
	return m, nil
}

func (m *topModel) View() tea.View {
	v := tea.NewView(top.Render(m.snap, m.dbs[m.filter], m.sortKey, m.width, m.height))
	v.AltScreen = true
	return v
}
