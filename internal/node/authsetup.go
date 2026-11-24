package node

import (
	"context"
	"time"

	"github.com/mbvlabs/vaire/internal/totp"
)

func handleAuthStatus(_ context.Context, env *Env) (Result, error) {
	status, err := operatorFactorStatus(env)
	if err != nil {
		return Result{}, err
	}

	return Result{Data: status}, nil
}

//nolint:gocognit // The setup checks and mutation stay within one state lock.
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

	if err := checkExposureRoot(); err != nil {
		return Result{}, err
	}

	s := env.Req.Secrets
	if p.Mode == "totp" && verifiedOperator(env) == "" {
		return Result{}, exposureFactorError("verified_operator_required", "a verified operator is required for authenticator setup")
	}

	if err := defaultExposureFactorGuard.forApproval(ctx, env, func(g exposureFactorGuard, st *exposureFactorState) error {
		if p.Mode != "password" && p.Mode != "totp" {
			return exposureFactorError("invalid_mode", "exposure factor mode must be password or totp")
		}

		if g.now().Before(st.LockedUntil) {
			return exposureFactorError("locked", "exposure factor is temporarily locked")
		}

		code := s["exposure_totp"]
		if p.Mode == "password" && st.PasswordHash != "" {
			code = ""
		}

		// A verified operator may enroll their own first authenticator without
		// knowing the shared password. Recheck under the lock for rotations.
		if p.Mode != "totp" || st.Secret != "" {
			if err := g.authorizeSetup(st, s["exposure_password"], code); err != nil {
				return err
			}
		}

		if p.Mode == "password" {
			if err := enrollExposurePassword(st, s["new_password"]); err != nil {
				return err
			}
		} else if err := g.setupTOTP(st, s["new_totp_secret"], s["new_totp_code"]); err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		st.Failures, st.LockedUntil = 0, time.Time{}

		return g.save(*st)
	}); err != nil {
		return Result{}, err
	}

	return Result{}, nil
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
