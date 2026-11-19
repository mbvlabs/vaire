package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"time"

	"github.com/mbvlabs/vaire/internal/totp"
)

const factorSetupCodeLifetime = 10 * time.Minute

// IssueFactorSetupCode issues a single-use initial enrollment code to a
// trusted root session. Issuing another code invalidates the previous one.
func IssueFactorSetupCode(ctx context.Context) (string, error) {
	if !trustedHostAdmin() {
		return "", trustedHostAdminRequired()
	}

	return defaultExposureFactorGuard.issueSetupCode(ctx)
}

func setupCodeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func (g exposureFactorGuard) issueSetupCode(ctx context.Context) (string, error) {
	var code string

	err := g.locked(ctx, func() error {
		st, err := g.load()
		if err != nil {
			return err
		}

		if st.PasswordHash != "" || st.Secret != "" {
			return exposureFactorError("factor_already_initialized", "an exposure factor is already enrolled")
		}

		if g.now().Before(st.LockedUntil) {
			return exposureFactorError("locked", "exposure factor is temporarily locked")
		}

		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}

		code = hex.EncodeToString(raw)
		st.SetupCodeHash, st.SetupCodeExpires = setupCodeHash(code), g.now().Add(factorSetupCodeLifetime)

		return g.save(st)
	})
	if err != nil {
		return "", err
	}

	return code, nil
}

func handleAuthStatus(_ context.Context, _ *Env) (Result, error) {
	status, err := ReadExposureFactorStatus()
	if err != nil {
		return Result{}, err
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
	if err := defaultExposureFactorGuard.setup(ctx, p.Mode, s["setup_code"], s["exposure_password"], s["exposure_totp"], s["new_password"], s["new_totp_secret"], s["new_totp_code"]); err != nil {
		return Result{}, err
	}

	return Result{}, nil
}

func (g exposureFactorGuard) setup(ctx context.Context, mode, setupCode, existingPassword, existingCode, newPassword, newSecret, newCode string) error {
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

		if err := g.authorizeSetup(&st, setupCode, existingPassword, existingCode); err != nil {
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

		st.SetupCodeHash, st.SetupCodeExpires = "", time.Time{}
		st.Failures, st.LockedUntil = 0, time.Time{}

		return g.save(st)
	})
}

func (g exposureFactorGuard) authorizeSetup(st *exposureFactorState, code, password, totpCode string) error {
	if st.PasswordHash != "" || st.Secret != "" {
		return g.verify(st, password, totpCode)
	}

	if st.SetupCodeHash == "" || !g.now().Before(st.SetupCodeExpires) ||
		subtle.ConstantTimeCompare([]byte(st.SetupCodeHash), []byte(setupCodeHash(code))) != 1 {
		return g.failure(st, "setup_code_invalid")
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
