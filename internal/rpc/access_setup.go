package rpc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const AccessSetupPrefix = "vaire-invite:"
const AccessEnrollPort = 6418
const AccessSetupRecovery = "recovery"

// AccessSetup is a bearer credential for the temporary WireGuard peer. It
// includes connection pins and coordinates, never permanent private keys.
type AccessSetup struct {
	Version             int       `json:"v"`
	Mode                string    `json:"mode,omitempty"`
	ID                  string    `json:"id"`
	Token               string    `json:"token"`
	Server              string    `json:"server"`
	Node                string    `json:"node"`
	Domain              string    `json:"domain,omitempty"`
	TLSMode             string    `json:"tls_mode,omitempty"`
	User                string    `json:"user"`
	Role                string    `json:"role,omitempty"`
	Label               string    `json:"label"`
	UnixLogin           string    `json:"unix_login"`
	Address             string    `json:"address"`
	TemporaryAddress    string    `json:"temp_address"`
	TemporaryPrivateKey string    `json:"temp_key"`
	NodeAddress         string    `json:"node_address"`
	NodePublicAddress   string    `json:"node_public_address"`
	NodeWGPublicKey     string    `json:"node_wg_key"`
	HostPublicKey       string    `json:"host_key"`
	WGPort              int       `json:"wg_port"`
	ExpiresAt           time.Time `json:"expires_at"`
}

type AccessRedeemParams struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	SSHPublicKey string `json:"ssh_public_key"`
	WGPublicKey  string `json:"wg_public_key"`
}

type AccessBootstrapOwnerParams struct {
	User         string `json:"user"`
	SSHPublicKey string `json:"ssh_public_key"`
	WGPublicKey  string `json:"wg_public_key"`
}

type AccessRecoveryIssueParams struct {
	User                string `json:"user"`
	Token               string `json:"-"`
	TemporaryPrivateKey string `json:"-"`
}

func (p AccessRecoveryIssueParams) Secrets() map[string]string {
	return map[string]string{"recovery_token": p.Token, "recovery_wg_key": p.TemporaryPrivateKey}
}

func (s AccessSetup) Encode() (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}

	return AccessSetupPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func DecodeAccessSetup(code string) (AccessSetup, error) {
	badSetup := errors.New("invalid Vaire setup code")

	encoded, ok := strings.CutPrefix(strings.TrimSpace(code), AccessSetupPrefix)
	if !ok || len(encoded) > 8192 {
		return AccessSetup{}, badSetup
	}

	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return AccessSetup{}, badSetup
	}

	var setup AccessSetup
	if err := json.Unmarshal(raw, &setup); err != nil || setup.Version != 1 || setup.ID == "" ||
		(setup.Mode != "" && setup.Mode != AccessSetupRecovery) ||
		(setup.Mode == "" && setup.ExpiresAt.IsZero()) ||
		(setup.Mode == AccessSetupRecovery && !setup.ExpiresAt.IsZero()) {
		return AccessSetup{}, badSetup
	}

	return setup, nil
}
