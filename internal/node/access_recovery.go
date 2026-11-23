package node

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/transport"
)

// Recovery credentials are supplied by the caller so a lost reply can be
// retried without storing the bearer token or private key on the node.
func handleRecoveryIssue(ctx context.Context, env *Env) (Result, error) {
	n, err := accessNode(env.Cluster)
	if err != nil {
		return Result{}, err
	}

	bootstrap := env.DeviceIdentity == nil
	if err := authorizeRecoveryIssue(env, n.Name, bootstrap); err != nil {
		return Result{}, err
	}

	var p rpc.AccessRecoveryIssueParams
	if env.Req.DryRun || json.Unmarshal(env.Req.Params, &p) != nil {
		return Result{}, accessInvalid("invalid recovery issuance request")
	}

	p.Token, p.TemporaryPrivateKey = env.Req.Secrets["recovery_token"], env.Req.Secrets["recovery_wg_key"]

	token, err := base64.RawURLEncoding.Strict().DecodeString(p.Token)
	if err != nil || len(token) != 32 || base64.RawURLEncoding.EncodeToString(token) != p.Token {
		return Result{}, accessInvalid("invalid recovery token")
	}

	public, err := transport.WGPublicKey(p.TemporaryPrivateKey)
	if err != nil || !config.ValidAccessWGKey(public) {
		return Result{}, accessInvalid("invalid recovery WireGuard key")
	}

	r, err := userRegistry()
	if err != nil {
		return Result{}, err
	}

	index := accessOperator(r, p.User)
	if !recoveryOwnerAllowed(r, index, bootstrap) {
		return Result{}, deviceAccessDenied()
	}

	digest := sha256.Sum256([]byte(p.Token))
	verifier := hex.EncodeToString(digest[:])

	match, err := recoveryIssuanceMatch(r, r.Operators[index].ID, verifier, public, bootstrap)
	if err != nil {
		return Result{}, err
	}

	if match < 0 {
		match, err = createRecoveryIssue(ctx, env, n, &r, index, verifier, public)
		if err != nil {
			return Result{}, err
		}
	}

	// Complete any interrupted rotation before publishing the new code.
	if err := cleanupRevokedRecovery(ctx, env, &r); err != nil {
		return Result{}, err
	}

	d := r.Devices[match]

	if err := prepareTemporaryAccess(ctx, n.WGAddress); err != nil {
		return Result{}, err
	}

	if err := addTemporaryAccessPeer(ctx, n.WGAddress, d.TemporaryAddress, d.TemporaryPublicKey); err != nil {
		return Result{}, err
	}

	setup, err := permanentSetup(env.Cluster, n, p.User, d)
	if err != nil {
		return Result{}, err
	}

	setup.Mode, setup.Role = rpc.AccessSetupRecovery, config.AccessRoleOwner
	setup.Token, setup.TemporaryAddress, setup.TemporaryPrivateKey = p.Token, d.TemporaryAddress, p.TemporaryPrivateKey

	code, err := setup.Encode()
	if err != nil {
		return Result{}, err
	}

	row := deviceReply(d, p.User)
	row.SetupCode = code

	return Result{Data: row}, nil
}

func recoveryOwnerAllowed(r config.AccessRegistry, index int, bootstrap bool) bool {
	return index >= 0 && !r.Operators[index].Revoked && r.Operators[index].Role == config.AccessRoleOwner &&
		(!bootstrap || r.Operators[index].BootstrapOwner)
}

func cleanupRevokedRecovery(ctx context.Context, env *Env, r *config.AccessRegistry) error {
	for i := range r.Devices {
		if r.Devices[i].Recovery && r.Devices[i].State == config.DeviceRevoked && r.Devices[i].Cleanup {
			if err := reconcileAccessDevice(ctx, env.Cluster, *r, &r.Devices[i]); err != nil {
				return err
			}
		}
	}

	return nil
}

func authorizeRecoveryIssue(env *Env, nodeName string, bootstrap bool) error {
	if !bootstrap || !env.TrustedHostAdmin {
		if err := authorizeUserManagement(env); err != nil {
			return err
		}
	}

	if bootstrap {
		return authorizeBootstrapOperation(env.Cluster.Name(), nodeName, env.Req.OperationID)
	}

	return nil
}

func createRecoveryIssue(ctx context.Context, env *Env, n config.NodeSpec, r *config.AccessRegistry, index int, verifier, public string) (int, error) {
	d, _, err := issueDeviceSetup(env.Cluster, n, *r, r.Operators[index], "recovery")
	if err != nil {
		return -1, err
	}

	d.Label = "recovery-" + strings.TrimPrefix(d.ID, "dev-")
	d.Recovery, d.ExpiresAt = true, time.Time{}
	d.RequestDigest, d.TemporaryPublicKey = verifier, public

	revokeUnusedRecovery(r)

	if err := r.AddDevice(d); err != nil {
		return -1, accessInvalid(err.Error())
	}

	if err := prepareTemporaryAccess(ctx, n.WGAddress); err != nil {
		return -1, err
	}

	if err := saveAccessRegistry(*r); err != nil {
		return -1, err
	}

	return len(r.Devices) - 1, nil
}

func revokeUnusedRecovery(r *config.AccessRegistry) {
	for i := range r.Devices {
		old := &r.Devices[i]
		if old.Recovery && old.State == config.DevicePending && old.WGPublicKey == "" {
			old.State, old.Cleanup = config.DeviceRevoked, true
		}
	}
}

func recoveryIssuanceMatch(r config.AccessRegistry, operator, verifier, public string, bootstrap bool) (int, error) {
	match := -1

	for i, d := range r.Devices {
		if !d.Recovery {
			continue
		}

		if d.RequestDigest == verifier {
			if d.OperatorID != operator || d.TemporaryPublicKey != public ||
				d.State != config.DevicePending || d.WGPublicKey != "" {
				return -1, deviceAccessDenied()
			}

			match = i
		} else if bootstrap {
			// Bootstrap can resume its original issuance, never replace it.
			return -1, deviceAccessDenied()
		}
	}

	return match, nil
}
