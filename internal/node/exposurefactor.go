package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/password"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/totp"
)

// Exposure factors are independent of database protection and secrets export.
const exposureFactorDir = "/etc/vaire/exposure-factor"

type exposureFactorState struct {
	PasswordHash string                    `json:"password_hash,omitempty"`
	Secret       string                    `json:"secret,omitempty"`
	LastStep     int64                     `json:"last_step,omitempty"`
	Failures     int                       `json:"failures,omitempty"`
	LockedUntil  time.Time                 `json:"locked_until,omitzero"`
	Operators    map[string]operatorFactor `json:"operators,omitempty"`
}

type operatorFactor struct {
	Secret      string    `json:"secret,omitempty"`
	LastStep    int64     `json:"last_step,omitempty"`
	Failures    int       `json:"failures,omitempty"`
	LockedUntil time.Time `json:"locked_until,omitzero"`
}

func verifiedOperator(env *Env) string {
	if env != nil && env.DeviceIdentity != nil && env.DeviceIdentity.IdentityKind == rpc.IdentityVerifiedDevice {
		return env.DeviceIdentity.OperatorID
	}

	return ""
}

type exposureFactorGuard struct {
	dir      string
	now      func() time.Time
	parent   *exposureFactorState
	operator string
}

var defaultExposureFactorGuard = exposureFactorGuard{dir: exposureFactorDir, now: time.Now}
var checkExposureRoot = exposureRoot

func exposureFactorError(reason, message string) error {
	hint := ""
	if reason == "factor_not_initialized" {
		hint = ExposureEnrollmentHint
	}

	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition, hint).
		WithDetails(map[string]any{"reason": reason})
}

// ExposureEnrollmentHint points to the trusted first-enrollment path.
const ExposureEnrollmentHint = "Run vaire auth setup from your workstation and select Master password or Authenticator code (TOTP)."

// ExposureFactorStatus reports enrolled methods without returning secrets.
type ExposureFactorStatus struct {
	Password   bool   `json:"password"`
	TOTP       bool   `json:"totp"`
	LegacyTOTP bool   `json:"legacy_totp,omitempty"`
	Operator   string `json:"operator,omitempty"`
}

func operatorFactorStatus(env *Env) (ExposureFactorStatus, error) {
	st, err := defaultExposureFactorGuard.load()
	id := verifiedOperator(env)

	status := ExposureFactorStatus{Password: st.PasswordHash != "", TOTP: id != "" && st.Operators[id].Secret != ""}
	if id != "" {
		status.Operator = env.DeviceIdentity.Operator
	}

	return status, err
}

// ReadExposureFactorStatus reads the atomically replaced node state without
// creating it. The result is advisory; enrollment and approval recheck it.
func ReadExposureFactorStatus() (ExposureFactorStatus, error) {
	st, err := defaultExposureFactorGuard.load()

	return ExposureFactorStatus{Password: st.PasswordHash != "", TOTP: st.Secret != ""}, err
}

func exposureRoot() error {
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return exposureFactorError("root_required", "exposure factor operations require real and effective root")
	}

	return nil
}

// locked serializes both processes and goroutines across the entire operation,
// including enrollment confirmation. The private directory protects the lock
// and the atomic state replacement from unprivileged access.
func (g exposureFactorGuard) locked(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return err
	}

	info, err := os.Lstat(g.dir)
	if err != nil {
		return err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || int64(stat.Uid) != int64(os.Geteuid()) {
		return exposureFactorError("unsafe_storage", "exposure factor directory must be private and owned by the current user")
	}

	f, err := os.OpenFile(filepath.Join(g.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	if err := ctx.Err(); err != nil {
		return err
	}

	return fn()
}

func (g exposureFactorGuard) load() (exposureFactorState, error) {
	var st exposureFactorState

	raw, err := os.ReadFile(filepath.Join(g.dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}

	if err != nil {
		return st, err
	}

	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("invalid exposure factor state: %w", err)
	}

	if (st.PasswordHash != "" && !password.Valid(st.PasswordHash)) || st.Failures < 0 || st.Failures >= protectMaxFailures {
		return st, exposureFactorError("invalid_state", "invalid exposure factor state")
	}

	if st.Secret != "" {
		if _, err := totp.DecodeSecret(st.Secret); err != nil {
			return st, exposureFactorError("invalid_state", "invalid exposure factor state")
		}
	}

	for id, factor := range st.Operators {
		if id == "" || factor.Failures < 0 || factor.Failures >= protectMaxFailures {
			return st, exposureFactorError("invalid_state", "invalid exposure factor state")
		}

		if factor.Secret != "" {
			if _, err := totp.DecodeSecret(factor.Secret); err != nil {
				return st, exposureFactorError("invalid_state", "invalid exposure factor state")
			}
		}
	}

	return st, nil
}

// operatorState returns a view whose TOTP and lockout belong only to this
// operator. The shared password remains in the parent state.
func operatorState(st exposureFactorState, id string) exposureFactorState {
	f := st.Operators[id]
	return exposureFactorState{PasswordHash: st.PasswordHash, Secret: f.Secret, LastStep: f.LastStep, Failures: f.Failures, LockedUntil: f.LockedUntil}
}

func putOperator(st *exposureFactorState, id string, view exposureFactorState) {
	if st.Operators == nil {
		st.Operators = make(map[string]operatorFactor)
	}

	st.Operators[id] = operatorFactor{Secret: view.Secret, LastStep: view.LastStep, Failures: view.Failures, LockedUntil: view.LockedUntil}
	st.PasswordHash = view.PasswordHash
}

func (g exposureFactorGuard) save(st exposureFactorState) error {
	if g.parent != nil {
		if g.operator == "" {
			g.parent.PasswordHash, g.parent.Failures, g.parent.LockedUntil = st.PasswordHash, st.Failures, st.LockedUntil
		} else {
			putOperator(g.parent, g.operator, st)
		}

		st = *g.parent
	}

	raw, err := json.Marshal(st) //nolint:gosec // G117: this is the root-only factor state file.
	if err != nil {
		return err
	}

	return engine.WriteFileAtomic(filepath.Join(g.dir, "state.json"), append(raw, '\n'), 0o600)
}

func (g exposureFactorGuard) forOperator(ctx context.Context, id string, fn func(exposureFactorGuard, *exposureFactorState) error) error {
	if id == "" {
		return exposureFactorError("verified_operator_required", "a verified operator is required")
	}

	return g.locked(ctx, func() error {
		st, err := g.load()
		if err != nil {
			return err
		}

		view := operatorState(st, id)
		g.parent, g.operator = &st, id

		return fn(g, &view)
	})
}

// forApproval uses an operator-specific TOTP view for verified devices.
// Authorized non-device callers get a shared-password-only view.
func (g exposureFactorGuard) forApproval(ctx context.Context, env *Env, fn func(exposureFactorGuard, *exposureFactorState) error) error {
	if id := verifiedOperator(env); id != "" {
		return g.forOperator(ctx, id, fn)
	}

	if env == nil || env.DeviceIdentity != nil {
		return exposureFactorError("verified_operator_required", "a verified operator is required for authenticator codes")
	}

	return g.locked(ctx, func() error {
		st, err := g.load()
		if err != nil {
			return err
		}

		view := exposureFactorState{PasswordHash: st.PasswordHash, Failures: st.Failures, LockedUntil: st.LockedUntil}
		g.parent = &st

		return fn(g, &view)
	})
}

func verifyOperatorApproval(ctx context.Context, env *Env) error {
	if err := checkExposureRoot(); err != nil {
		return err
	}

	return defaultExposureFactorGuard.forApproval(ctx, env, func(g exposureFactorGuard, st *exposureFactorState) error {
		code := ""
		if verifiedOperator(env) != "" {
			code = env.Req.Secrets["exposure_totp"]
		}

		return g.verify(st, env.Req.Secrets["exposure_password"], code)
	})
}

func (g exposureFactorGuard) failure(st *exposureFactorState, reason string) error {
	st.Failures++
	if st.Failures >= protectMaxFailures {
		st.Failures = 0
		st.LockedUntil = g.now().Add(protectLockout)
		reason = "locked"
	}

	if err := g.save(*st); err != nil {
		return err
	}

	return exposureFactorError(reason, "exposure factor rejected")
}

func (g exposureFactorGuard) verify(st *exposureFactorState, pw, code string) error {
	if g.now().Before(st.LockedUntil) {
		return exposureFactorError("locked", "exposure factor is temporarily locked")
	}

	if st.PasswordHash == "" && st.Secret == "" {
		return exposureFactorError("factor_not_initialized", "no exposure factor is enrolled")
	}

	switch {
	case pw != "" && st.PasswordHash != "":
		if !password.Verify(st.PasswordHash, pw) {
			return g.failure(st, "factor_invalid")
		}
	case code != "" && st.Secret != "":
		key, err := totp.DecodeSecret(st.Secret)
		if err != nil {
			return exposureFactorError("invalid_state", "invalid exposure factor state")
		}

		step, err := totp.Verify(key, code, g.now(), st.LastStep)
		if err != nil {
			reason := "factor_invalid"
			if errors.Is(err, totp.ErrReused) {
				reason = "code_reused"
			}

			return g.failure(st, reason)
		}

		st.LastStep = step
	default:
		return g.failure(st, "factor_invalid")
	}

	st.Failures, st.LockedUntil = 0, time.Time{}

	return g.save(*st)
}

func (g exposureFactorGuard) check(ctx context.Context, password, code string) error {
	return g.locked(ctx, func() error {
		st, err := g.load()
		if err != nil {
			return err
		}

		return g.verify(&st, password, code)
	})
}

// EnrollExposureFactor enrolls or replaces one exposure-only factor. The first
// enrollment trusts the root console caller; later changes require an existing
// factor. mode is "password" or "totp". The callback receives the new raw
// base32 secret and must return its current code.
func EnrollExposureFactor(ctx context.Context, mode, existingPassword, existingCode, newPassword string, confirmTOTP func(secret string) (string, error)) error {
	if err := exposureRoot(); err != nil {
		return err
	}

	return defaultExposureFactorGuard.enroll(ctx, mode, existingPassword, existingCode, newPassword, confirmTOTP)
}

func (g exposureFactorGuard) enroll(ctx context.Context, mode, existingPassword, existingCode, newPassword string, confirmTOTP func(string) (string, error)) error {
	if mode != "password" && mode != "totp" {
		return exposureFactorError("invalid_mode", "exposure factor mode must be password or totp")
	}

	return g.locked(ctx, func() error {
		st, err := g.load()
		if err != nil {
			return err
		}

		if g.now().Before(st.LockedUntil) {
			return exposureFactorError("locked", "exposure factor is temporarily locked")
		}

		if st.PasswordHash != "" || st.Secret != "" {
			if err := g.verify(&st, existingPassword, existingCode); err != nil {
				return err
			}
		}

		if mode == "password" {
			err = enrollExposurePassword(&st, newPassword)
		} else {
			err = g.enrollTOTP(ctx, &st, confirmTOTP)
		}

		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		st.Failures, st.LockedUntil = 0, time.Time{}

		return g.save(st)
	})
}

func enrollExposurePassword(st *exposureFactorState, newPassword string) error {
	hash, err := password.Hash(newPassword)
	if errors.Is(err, password.ErrTooShort) {
		return exposureFactorError("password_too_short", fmt.Sprintf("exposure password must be at least %d characters", password.MinLength))
	}

	if err != nil {
		return err
	}

	st.PasswordHash = hash

	return nil
}

func (g exposureFactorGuard) enrollTOTP(ctx context.Context, st *exposureFactorState, confirm func(string) (string, error)) error {
	if confirm == nil {
		return exposureFactorError("confirmation_required", "TOTP confirmation is required")
	}

	secret, err := totp.NewSecret()
	if err != nil {
		return err
	}

	code, err := confirm(secret)
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	key, err := totp.DecodeSecret(secret)
	if err != nil {
		return err
	}

	step, err := totp.Verify(key, code, g.now(), 0)
	if err != nil {
		return g.failure(st, "confirmation_invalid")
	}

	st.Secret, st.LastStep = secret, step

	return nil
}
