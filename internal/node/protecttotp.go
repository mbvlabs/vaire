package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/totp"
)

// ProtectStatePath holds the TOTP secret that lifts a database's protection,
// with its replay and lockout state. Root only (0600), and outside
// cluster.toml, which operators read and pull.
const ProtectStatePath = "/etc/vaire/protect.json"

// protectStateMode is protect.json's mode; root owns it.
const protectStateMode os.FileMode = 0o600

// ProtectCodeSecret is the request secret that carries a TOTP code. As a
// secret it is redacted from every output and never written to a run log.
const ProtectCodeSecret = "protect_code"

const (
	// protectMaxFailures wrong codes in a row lock the codes out.
	protectMaxFailures = 5
	// protectLockout is how long a lockout lasts.
	protectLockout = 15 * time.Minute
	// protectIssuer names vaire in the authenticator app.
	protectIssuer = "vaire"
)

// protectState is ProtectStatePath.
type protectState struct {
	// Secret is the confirmed base32 secret; empty until protect.confirm.
	Secret string `json:"secret,omitempty"`
	// Pending is a secret protect.init made that is not confirmed yet.
	Pending     string    `json:"pending,omitempty"`
	ConfirmedAt time.Time `json:"confirmed_at,omitzero"`
	// LastStep is the time step of the last code accepted for Secret; a code
	// for it or an earlier step is refused.
	LastStep    int64     `json:"last_step,omitempty"`
	Failures    int       `json:"failures,omitempty"`
	LockedUntil time.Time `json:"locked_until,omitzero"`
}

// protectGuard reads and writes the TOTP state. Tests point it at a temp
// file and a fixed clock.
type protectGuard struct {
	path string
	now  func() time.Time
}

var defaultProtectGuard = protectGuard{path: ProtectStatePath, now: time.Now}

func (g protectGuard) load() (protectState, error) {
	var st protectState
	raw, err := os.ReadFile(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("%s does not parse: %w", g.path, err)
	}
	return st, nil
}

func (g protectGuard) save(st protectState) error {
	raw, err := json.MarshalIndent(st, "", "  ") //nolint:gosec // G117: this is the root-only file that holds the secret.
	if err != nil {
		return err
	}
	if err := engine.WriteFileAtomic(g.path, append(raw, '\n'), protectStateMode); err != nil {
		return err
	}
	if g.path == ProtectStatePath {
		masterbackup.MarkDirty("protect.json")
	}
	return nil
}

// configured reports whether a confirmed TOTP secret exists.
func (g protectGuard) configured() (bool, error) {
	st, err := g.load()
	return st.Secret != "", err
}

// verify checks code against the confirmed secret, or with pending against
// the pending one, and saves the outcome: the accepted step, or one more
// failure, locking codes out for protectLockout after protectMaxFailures.
// It returns the state as saved.
func (g protectGuard) verify(code string, pending bool) (protectState, error) {
	st, err := g.load()
	if err != nil {
		return st, err
	}
	now := g.now()
	if now.Before(st.LockedUntil) {
		return st, protectLocked(st.LockedUntil)
	}
	secret, last := st.Secret, st.LastStep
	if pending {
		// A pending secret has never accepted a code.
		secret, last = st.Pending, 0
	}
	if strings.TrimSpace(code) == "" {
		return st, protectCodeError("a code from the authenticator app is required", "code_required",
			"Run the command yourself at a terminal; it asks for the current 6-digit code.", nil)
	}
	key, err := totp.DecodeSecret(secret)
	if err != nil {
		return st, err
	}
	step, verr := totp.Verify(key, code, now, last)
	if verr != nil {
		st.Failures++
		left := protectMaxFailures - st.Failures
		if st.Failures >= protectMaxFailures {
			st.Failures, st.LockedUntil = 0, now.Add(protectLockout)
		}
		if err := g.save(st); err != nil {
			return st, err
		}
		if !st.LockedUntil.IsZero() && now.Before(st.LockedUntil) {
			return st, protectLocked(st.LockedUntil)
		}
		reason := "code_invalid"
		if errors.Is(verr, totp.ErrReused) {
			reason = "code_reused"
		}
		return st, protectCodeError(verr.Error(), reason,
			"Wait for the next code in the authenticator app and try again. "+lockoutWarning(left, protectLockout),
			map[string]any{"attempts_left": left})
	}
	st.Failures, st.LockedUntil = 0, time.Time{}
	if pending {
		st.Secret, st.Pending, st.ConfirmedAt = st.Pending, "", now.UTC()
	}
	st.LastStep = step
	return st, g.save(st)
}

// lockoutWarning says how many more wrong codes start a lockout, and for
// how long: "1 more wrong code locks codes out for 15 minutes."
func lockoutWarning(left int, lockout time.Duration) string {
	if left == 1 {
		return "1 more wrong code locks codes out for " + humanMinutes(lockout) + "."
	}
	return fmt.Sprintf("%d more wrong codes lock codes out for %s.", left, humanMinutes(lockout))
}

// humanMinutes writes a whole-minute duration in words: "15 minutes",
// "1 minute", "2 hours". Other durations keep Go's form.
func humanMinutes(d time.Duration) string {
	unit, n := "minute", int(d/time.Minute)
	if d%time.Hour == 0 {
		unit, n = "hour", int(d/time.Hour)
	}
	if d <= 0 || d%time.Minute != 0 {
		return d.String()
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

func protectCodeError(message, reason, hint string, extra map[string]any) error {
	details := map[string]any{"reason": reason}
	for k, v := range extra {
		details[k] = v
	}
	return output.NewError(output.CodeProtectCodeRejected, message, output.ExitUnsafe, hint).WithDetails(details)
}

func protectLocked(until time.Time) error {
	return protectCodeError("too many wrong codes; codes are locked out until "+until.UTC().Format(time.RFC3339), "locked",
		"Wait until the lockout ends, then try again with a fresh code. vaire protect status shows it.",
		map[string]any{"locked_until": until.UTC().Format(time.RFC3339)})
}

func protectNotInitialized(code, message string, exit int) error {
	return output.NewError(code, message, exit,
		"A human runs vaire protect init first, at a terminal, and scans the QR code with an authenticator app. Lifting a protection then needs a current code from it.").
		WithDetails(map[string]any{"reason": "protect_not_initialized"})
}

// newlyProtected lists the databases protected in updated that were not in
// old.
func newlyProtected(old, updated config.Cluster) []string {
	var out []string
	for _, c := range updated.Clusters {
		if !c.Protected {
			continue
		}
		if o, ok := old.ClusterByName(c.Name); !ok || !o.Protected {
			out = append(out, c.Name)
		}
	}
	return out
}

// checkProtect refuses to protect names while no TOTP secret is confirmed:
// a database must never be protected without a way to lift it.
func (g protectGuard) checkProtect(names []string) error {
	if len(names) == 0 {
		return nil
	}
	ok, err := g.configured()
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return protectNotInitialized(output.CodePrecondition,
		"protecting "+strings.Join(names, ", ")+" needs two-factor protection set up first", output.ExitPrecondition)
}

// checkUnprotect refuses a config.set that lifts a database's protection (or
// removes a protected entry) unless the request carries a current TOTP code
// in its protect_code secret. The code proves a human holding the
// authenticator approved it; a typed word would not, since an agent at a
// terminal can type too.
func (g protectGuard) checkUnprotect(old, updated config.Cluster, code string) error {
	lifted := Unprotected(old, updated)
	if len(lifted) == 0 {
		return nil
	}
	ok, err := g.configured()
	if err != nil {
		return err
	}
	if !ok {
		return protectNotInitialized(output.CodeDatabaseProtected,
			"lifting the protection of "+strings.Join(lifted, ", ")+" needs two-factor protection, which this node does not have set up", output.ExitUnsafe)
	}
	_, err = g.verify(code, false)
	return err
}

type protectInitParams struct {
	Rotate bool   `json:"rotate"`
	Label  string `json:"label"`
}

// handleProtectInit makes a new TOTP secret and keeps it pending until
// protect.confirm sees a code from it. With a secret already confirmed it
// refuses unless rotate, and rotating needs a current code from the old one.
// The secret is in the result, so the method writes no run log.
func handleProtectInit(_ context.Context, env *Env) (Result, error) {
	return defaultProtectGuard.init(env)
}

func (g protectGuard) init(env *Env) (Result, error) {
	var p protectInitParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	st, err := g.load()
	if err != nil {
		return Result{}, err
	}
	switch {
	case st.Secret != "" && !p.Rotate:
		return Result{}, output.NewError(output.CodePrecondition, "two-factor protection is already set up on this node", output.ExitPrecondition,
			"To replace the secret, run vaire protect init --rotate; it asks for a current code from the existing one.").
			WithDetails(map[string]any{"reason": "protect_already_initialized"})
	case st.Secret == "" && p.Rotate:
		return Result{}, output.NewError(output.CodePrecondition, "two-factor protection is not set up, so there is nothing to rotate", output.ExitPrecondition,
			"Run vaire protect init without --rotate.").
			WithDetails(map[string]any{"reason": "protect_not_initialized"})
	case p.Rotate:
		if st, err = g.verify(env.Req.Secrets[ProtectCodeSecret], false); err != nil {
			return Result{}, err
		}
	}
	secret, err := totp.NewSecret()
	if err != nil {
		return Result{}, err
	}
	st.Pending = secret
	if err := g.save(st); err != nil {
		return Result{}, err
	}
	label := strings.TrimSpace(p.Label)
	if label == "" {
		label, _ = os.Hostname()
	}
	return Result{Data: map[string]any{
		"secret": secret,
		"uri":    totp.URI(protectIssuer, label, secret),
		"rotate": p.Rotate,
	}}, nil
}

// handleProtectConfirm checks a code from the pending secret and makes it
// the confirmed one.
func handleProtectConfirm(_ context.Context, env *Env) (Result, error) {
	return defaultProtectGuard.confirm(env)
}

func (g protectGuard) confirm(env *Env) (Result, error) {
	st, err := g.load()
	if err != nil {
		return Result{}, err
	}
	if st.Pending == "" {
		return Result{}, output.NewError(output.CodePrecondition, "there is no pending two-factor setup to confirm", output.ExitPrecondition,
			"Run vaire protect init first.").WithDetails(map[string]any{"reason": "protect_not_pending"})
	}
	rotated := st.Secret != ""
	if st, err = g.verify(env.Req.Secrets[ProtectCodeSecret], true); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"configured": true, "rotated": rotated, "confirmed_at": st.ConfirmedAt.Format(time.RFC3339)}}, nil
}

// protectStatus is the protect.status result.
type protectStatus struct {
	Configured  bool     `json:"configured"`
	Pending     bool     `json:"pending"`
	ConfirmedAt string   `json:"confirmed_at,omitempty"`
	Failures    int      `json:"failures"`
	LockedUntil string   `json:"locked_until,omitempty"`
	Protected   []string `json:"protected_databases"`
	// Databases is every database in cluster.toml with its protection.
	Databases []ProtectedDatabase `json:"databases"`
}

// ProtectedDatabase is one database in the protect.status result.
type ProtectedDatabase struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
}

func handleProtectStatus(_ context.Context, _ *Env) (Result, error) {
	return defaultProtectGuard.status(config.ClusterPath)
}

// protectVerify is protect.verify's result.
type protectVerify struct {
	Verified bool `json:"verified"`
}

// handleProtectVerify checks a code from the authenticator app without
// changing anything: the node-side check the vaire secrets export gate uses.
// It reuses the write path's guard, so a code is one use and failures lock
// codes out as they do there.
func handleProtectVerify(_ context.Context, env *Env) (Result, error) {
	ok, err := defaultProtectGuard.configured()
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, protectNotInitialized(output.CodePrecondition,
			"this node has no two-factor protection set up", output.ExitPrecondition)
	}
	if _, err := defaultProtectGuard.verify(env.Req.Secrets[ProtectCodeSecret], false); err != nil {
		return Result{}, err
	}
	return Result{Data: protectVerify{Verified: true}}, nil
}

func (g protectGuard) status(clusterPath string) (Result, error) {
	st, err := g.load()
	if err != nil {
		return Result{}, err
	}
	out := protectStatus{Configured: st.Secret != "", Pending: st.Pending != "", Failures: st.Failures,
		Protected: []string{}, Databases: []ProtectedDatabase{}}
	if !st.ConfirmedAt.IsZero() {
		out.ConfirmedAt = st.ConfirmedAt.UTC().Format(time.RFC3339)
	}
	if g.now().Before(st.LockedUntil) {
		out.LockedUntil = st.LockedUntil.UTC().Format(time.RFC3339)
	}
	if cfg, err := config.LoadCluster(clusterPath); err == nil {
		out.Protected, out.Databases = protectionOf(cfg)
		// An open copy of a protected database (<db>__restoring) holds the
		// live database's roles, so writing to it needs a code too. It is
		// listed in Protected (the gate's list) but not Databases (which is
		// cluster.toml's own databases).
		out.Protected = append(out.Protected, protectedCopies(cfg)...)
	}
	return Result{Data: out}, nil
}

// protectedCopies lists the open copies (<db>__restoring) of a protected
// database, from the restores the server holds open. A copy is read-only
// until promote when it came from a backup, but a dump copy takes writes;
// both follow the live database's protection.
func protectedCopies(cfg config.Cluster) []string {
	recs, err := backup.LoadRecords()
	if err != nil {
		return nil
	}
	return copiesOf(cfg, recs)
}

// copiesOf is protectedCopies over records already read.
func copiesOf(cfg config.Cluster, recs backup.Records) []string {
	var out []string
	for db, r := range recs.Restores {
		if spec, ok := cfg.ClusterByName(db); ok && spec.Protected && r.Port != 0 {
			out = append(out, backup.RestoringName(db))
		}
	}
	sort.Strings(out)
	return out
}

// protectionOf lists the protected databases of cfg, and every database
// with its protection, in cluster.toml order.
func protectionOf(cfg config.Cluster) ([]string, []ProtectedDatabase) {
	protected, all := []string{}, make([]ProtectedDatabase, 0, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		all = append(all, ProtectedDatabase{Name: c.Name, Protected: c.Protected})
		if c.Protected {
			protected = append(protected, c.Name)
		}
	}
	return protected, all
}
