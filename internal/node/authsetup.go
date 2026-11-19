package node

import (
	"context"
	"time"

	"github.com/mbvlabs/vaire/internal/totp"
)

func handleAuthStatus(_ context.Context, _ *Env) (Result, error) {
	status, err := ReadExposureFactorStatus()
	if err != nil {
		return Result{}, err
	}

	if !status.Password && !status.TOTP {
		status.LegacyTOTP, err = defaultProtectGuard.configured()
		if err != nil {
			return Result{}, err
		}
	}

	return Result{Data: status}, nil
}

func handleAuthSetup(ctx context.Context, env *Env) (Result, error) {
	var p struct {
		Mode string `json:"mode"`
	}
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}

	if env.Req.DryRun {
		return Result{}, exposureFactorError("dry_run_unsupported", "factor setup cannot be a dry run")
	}

	if err := exposureRoot(); err != nil {
		return Result{}, err
	}

	s := env.Req.Secrets
	if err := authorizeLegacyFactorSetup(s[ProtectCodeSecret]); err != nil {
		return Result{}, err
	}

	if err := defaultExposureFactorGuard.setup(ctx, p.Mode, s["exposure_password"], s["exposure_totp"], s["new_password"], s["new_totp_secret"], s["new_totp_code"]); err != nil {
		return Result{}, err
	}

	return Result{}, nil
}

func authorizeLegacyFactorSetup(code string) error {
	ready, err := hasApprovalFactor()
	if err != nil || ready {
		return err
	}

	legacy, err := defaultProtectGuard.configured()
	if err != nil || !legacy {
		return err
	}

	_, err = defaultProtectGuard.verify(code, false)

	return err
}

func (g exposureFactorGuard) setup(ctx context.Context, mode, existingPassword, existingCode, newPassword, newSecret, newCode string) error {
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

		if err := g.authorizeSetup(&st, existingPassword, existingCode); err != nil {
			return err
		}

		if mode == "password" {
			if err := enrollExposurePassword(&st, newPassword); err != nil {
				return err
			}
		} else if err := g.setupTOTP(&st, newSecret, newCode); err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		st.Failures, st.LockedUntil = 0, time.Time{}

		return g.save(st)
	})
}

// The authenticated operator can set the first factor. Recheck under the
// state lock so concurrent first-time requests cannot replace it.
func (g exposureFactorGuard) authorizeSetup(st *exposureFactorState, password, totpCode string) error {
	if st.PasswordHash != "" || st.Secret != "" {
		return g.verify(st, password, totpCode)
	}

	return nil
}

func (g exposureFactorGuard) setupTOTP(st *exposureFactorState, secret, code string) error {
	key, err := totp.DecodeSecret(secret)
	if err != nil {
		return exposureFactorError("confirmation_invalid", "TOTP confirmation rejected")
	}

	step, err := totp.Verify(key, code, g.now(), 0)
	if err != nil {
		return g.failure(st, "confirmation_invalid")
	}

	st.Secret, st.LastStep = secret, step

	return nil
}
