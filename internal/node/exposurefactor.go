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
	"github.com/mbvlabs/vaire/internal/totp"
)

// Exposure factors are independent of database protection and secrets export.
const exposureFactorDir = "/etc/vaire/exposure-factor"

type exposureFactorState struct {
	PasswordHash string    `json:"password_hash,omitempty"`
	Secret       string    `json:"secret,omitempty"`
	LastStep     int64     `json:"last_step,omitempty"`
	Failures     int       `json:"failures,omitempty"`
	LockedUntil  time.Time `json:"locked_until,omitzero"`
}

type exposureFactorGuard struct {
	dir string
	now func() time.Time
}

var defaultExposureFactorGuard = exposureFactorGuard{dir: exposureFactorDir, now: time.Now}

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

func hasApprovalFactor() (bool, error) {
	st, err := ReadExposureFactorStatus()

	return st.Password || st.TOTP, err
}

func carriesApprovalFactor(values map[string]string) bool {
	return values["exposure_password"] != "" || values["exposure_totp"] != ""
}

func verifyApprovalFactor(ctx context.Context, values map[string]string) error {
	return verifyExposureFactor(ctx, values["exposure_password"], values["exposure_totp"])
}

// ExposureFactorStatus reports enrolled methods without returning secrets.
type ExposureFactorStatus struct {
	Password   bool `json:"password"`
	TOTP       bool `json:"totp"`
	LegacyTOTP bool `json:"legacy_totp,omitempty"`
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

	return st, nil
}

func (g exposureFactorGuard) save(st exposureFactorState) error {
	raw, err := json.Marshal(st) //nolint:gosec // G117: this is the root-only factor state file.
	if err != nil {
		return err
	}

	return engine.WriteFileAtomic(filepath.Join(g.dir, "state.json"), append(raw, '\n'), 0o600)
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

func verifyExposureFactor(ctx context.Context, password, code string) error {
	if err := exposureRoot(); err != nil {
		return err
	}

	return defaultExposureFactorGuard.check(ctx, password, code)
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
