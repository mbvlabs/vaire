package config

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/crypto/ssh"
)

// AccessDevicesPath is reserved for the root-owned device registry. This
// package does not create the file or enforce its ownership or permissions.
const AccessDevicesPath = "/etc/vaire/access-devices.toml"

const (
	AccessRoleOwner  = "owner"
	AccessRoleMember = "member"

	DevicePending = "pending"
	DeviceActive  = "active"
	DeviceRevoked = "revoked"
)

// AccessRegistry is separate from the legacy shared-key operator registry.
// Revoked records remain in the registry to preserve identity and prevent reuse.
type AccessRegistry struct {
	Operators []AccessOperator `toml:"operators"`
	Devices   []AccessDevice   `toml:"devices"`
}

type AccessOperator struct {
	ID   string `toml:"id"`
	Name string `toml:"name"`
	Role string `toml:"role"`
}

type AccessDevice struct {
	ID            string `toml:"id"`
	OperatorID    string `toml:"operator_id"`
	Label         string `toml:"label"`
	State         string `toml:"state"`
	SSHPublicKey  string `toml:"ssh_public_key"`
	WGPublicKey   string `toml:"wg_public_key"`
	Address       string `toml:"address"`
	UnixLogin     string `toml:"unix_login"`
	RequestDigest string `toml:"request_digest"`
}

var (
	accessIDPattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	unixLoginPattern   = regexp.MustCompile(`^vdev-[a-z0-9][a-z0-9_-]{0,25}$`)
	accessLabelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// Validate checks the entire registry, including revoked records. Keys,
// addresses, logins and request digests cannot be reused by another device.
func (r AccessRegistry) Validate() error {
	ids, names := map[string]bool{}, map[string]bool{}

	for _, op := range r.Operators {
		if !accessIDPattern.MatchString(op.ID) || !validAccessLabel(op.Name) {
			return fmt.Errorf("invalid operator ID or name")
		}

		if op.Role != AccessRoleOwner && op.Role != AccessRoleMember {
			return fmt.Errorf("operator %q: invalid role", op.ID)
		}

		if ids[op.ID] || names[op.Name] {
			return fmt.Errorf("duplicate operator ID or name: %q", op.ID)
		}

		ids[op.ID], names[op.Name] = true, true
	}

	deviceIDs, labels := map[string]bool{}, map[string]bool{}
	sshKeys, wgKeys := map[string]bool{}, map[string]bool{}
	addresses, logins, digests := map[string]bool{}, map[string]bool{}, map[string]bool{}

	for _, d := range r.Devices {
		if !accessIDPattern.MatchString(d.ID) || !ids[d.OperatorID] || !validAccessLabel(d.Label) {
			return fmt.Errorf("device %q: invalid ID, operator or label", d.ID)
		}

		key, err := validateAccessDevice(d)
		if err != nil {
			return err
		}

		label := d.OperatorID + "\x00" + d.Label
		sshKey := string(key.Marshal())

		if deviceIDs[d.ID] || labels[label] || sshKeys[sshKey] || wgKeys[d.WGPublicKey] ||
			addresses[d.Address] || logins[d.UnixLogin] || digests[d.RequestDigest] {
			return fmt.Errorf("device %q: duplicate identity or attribute", d.ID)
		}

		deviceIDs[d.ID], labels[label], sshKeys[sshKey], wgKeys[d.WGPublicKey] = true, true, true, true
		addresses[d.Address], logins[d.UnixLogin], digests[d.RequestDigest] = true, true, true
	}

	return nil
}

func validateAccessDevice(d AccessDevice) (ssh.PublicKey, error) {
	if d.State != DevicePending && d.State != DeviceActive && d.State != DeviceRevoked {
		return nil, fmt.Errorf("device %q: invalid state", d.ID)
	}

	key, err := parseAccessSSHKey(d.SSHPublicKey)
	if err != nil {
		return nil, fmt.Errorf("device %q: %w", d.ID, err)
	}

	wg, err := base64.StdEncoding.Strict().DecodeString(d.WGPublicKey)
	if err != nil || len(wg) != 32 || base64.StdEncoding.EncodeToString(wg) != d.WGPublicKey || bytes.Equal(wg, make([]byte, 32)) {
		return nil, fmt.Errorf("device %q: invalid WireGuard public key", d.ID)
	}

	addr, err := netip.ParseAddr(d.Address)
	if err != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() {
		return nil, fmt.Errorf("device %q: invalid IPv4 address", d.ID)
	}

	if !unixLoginPattern.MatchString(d.UnixLogin) {
		return nil, fmt.Errorf("device %q: invalid Unix login", d.ID)
	}

	digest, err := hex.DecodeString(d.RequestDigest)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != d.RequestDigest {
		return nil, fmt.Errorf("device %q: invalid SHA-256 request digest", d.ID)
	}

	return key, nil
}

func parseAccessSSHKey(value string) (ssh.PublicKey, error) {
	line := strings.TrimSpace(value)
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))

	if err != nil || strings.ContainsAny(line, "\r\n") || key.Type() != ssh.KeyAlgoED25519 || len(options) != 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, fmt.Errorf("invalid SSH Ed25519 public key")
	}

	return key, nil
}

// ValidateBareSSHEd25519PublicKey accepts only a canonical key without
// options, a comment, or surrounding whitespace.
func ValidateBareSSHEd25519PublicKey(value string) error {
	key, err := parseAccessSSHKey(value)
	if err != nil {
		return err
	}

	if value != strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(key)), "\n") {
		return fmt.Errorf("SSH Ed25519 public key is not bare and canonical")
	}

	return nil
}

func validAccessLabel(s string) bool {
	return accessLabelPattern.MatchString(s)
}

// AddOperator adds a distinct operator only if the resulting registry is valid.
func (r *AccessRegistry) AddOperator(op AccessOperator) error {
	next := *r
	next.Operators = append(append([]AccessOperator(nil), r.Operators...), op)

	if err := next.Validate(); err != nil {
		return err
	}

	*r = next

	return nil
}

// AddDevice rejects repeated requests and collisions without changing r.
func (r *AccessRegistry) AddDevice(d AccessDevice) error {
	next := *r
	next.Devices = append(append([]AccessDevice(nil), r.Devices...), d)

	if err := next.Validate(); err != nil {
		return err
	}

	*r = next

	return nil
}

// Encode returns validated TOML; callers are responsible for secure storage.
func (r AccessRegistry) Encode() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}

	return toml.Marshal(r)
}

// DecodeAccessRegistry parses and validates a registry without accessing disk.
func DecodeAccessRegistry(raw []byte) (AccessRegistry, error) {
	var r AccessRegistry

	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&r); err != nil {
		return AccessRegistry{}, err
	}

	if err := r.Validate(); err != nil {
		return AccessRegistry{}, err
	}

	return r, nil
}
