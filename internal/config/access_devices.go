package config

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/crypto/ssh"
)

// AccessDevicesPath is reserved for the root-owned device registry. This
// package does not create the file or enforce its ownership or permissions.
const AccessDevicesPath = "/etc/vaire/access-devices.toml"

const (
	AccessRoleOwner  = "admin"
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
	ID             string `toml:"id"`
	Name           string `toml:"name"`
	Role           string `toml:"role"`
	Revoked        bool   `toml:"revoked,omitempty"`
	BootstrapOwner bool   `toml:"bootstrap_owner,omitempty"`
}

type AccessDevice struct {
	ID                 string    `toml:"id"`
	OperatorID         string    `toml:"operator_id"`
	Label              string    `toml:"label"`
	State              string    `toml:"state"`
	SSHPublicKey       string    `toml:"ssh_public_key"`
	WGPublicKey        string    `toml:"wg_public_key"`
	Address            string    `toml:"address"`
	UnixLogin          string    `toml:"unix_login"`
	UnixUID            string    `toml:"unix_uid,omitempty"`
	RequestDigest      string    `toml:"request_digest"`
	TemporaryAddress   string    `toml:"temporary_address,omitempty"`
	TemporaryPublicKey string    `toml:"temporary_public_key,omitempty"`
	ExpiresAt          time.Time `toml:"expires_at,omitempty"`
	Recovery           bool      `toml:"recovery,omitempty"`
	// Cleanup records a peer which must be removed from the live host.
	Cleanup bool `toml:"cleanup,omitempty"`
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
	bootstrapOwner := false

	for _, op := range r.Operators {
		if !accessIDPattern.MatchString(op.ID) || !validAccessLabel(op.Name) {
			return fmt.Errorf("invalid operator ID or name")
		}

		if op.Role != AccessRoleOwner && op.Role != AccessRoleMember {
			return fmt.Errorf("operator %q: invalid role", op.ID)
		}

		if op.BootstrapOwner {
			if bootstrapOwner || op.Role != AccessRoleOwner {
				return fmt.Errorf("invalid initial owner")
			}

			bootstrapOwner = true
		}

		if ids[op.ID] || names[op.Name] {
			return fmt.Errorf("duplicate operator ID or name: %q", op.ID)
		}

		ids[op.ID], names[op.Name] = true, true
	}

	if err := validateRecoveryOwners(r); err != nil {
		return err
	}

	return validateAccessDevices(r.Devices, ids)
}

func validateRecoveryOwners(r AccessRegistry) error {
	for _, d := range r.Devices {
		if d.Recovery {
			owner := false
			for _, op := range r.Operators {
				owner = owner || (op.ID == d.OperatorID && op.Role == AccessRoleOwner)
			}

			if !owner {
				return fmt.Errorf("device %q: recovery requires an owner", d.ID)
			}
		}
	}

	return nil
}

func validateAccessDevices(devices []AccessDevice, ids map[string]bool) error {
	seen := make(map[[2]string]bool, len(devices)*9)
	for _, d := range devices {
		if !accessIDPattern.MatchString(d.ID) || !ids[d.OperatorID] || !validAccessLabel(d.Label) {
			return fmt.Errorf("device %q: invalid ID, operator or label", d.ID)
		}

		if err := validateAccessDevice(d); err != nil {
			return err
		}

		for _, key := range accessUniqueAttributes(d) {
			if seen[key] {
				return fmt.Errorf("device %q: duplicate %s", d.ID, key[0])
			}

			seen[key] = true
		}
	}

	return nil
}

func accessUniqueAttributes(d AccessDevice) [][2]string {
	keys := [][2]string{{"ID", d.ID}, {"address", d.Address}, {"Unix login", d.UnixLogin}, {"digest", d.RequestDigest}}
	if d.State != DeviceRevoked {
		keys = append(keys, [2]string{"device label", d.OperatorID + "\x00" + d.Label})
	}

	if d.SSHPublicKey != "" {
		key, _ := parseAccessSSHKey(d.SSHPublicKey)
		keys = append(keys, [2]string{"SSH key", string(key.Marshal())})
	}

	if d.WGPublicKey != "" {
		keys = append(keys, [2]string{"WireGuard key", d.WGPublicKey})
	}

	if d.UnixUID != "" {
		keys = append(keys, [2]string{"Unix UID", d.UnixUID})
	}

	if d.TemporaryAddress != "" {
		keys = append(keys, [2]string{"address", d.TemporaryAddress}, [2]string{"WireGuard key", d.TemporaryPublicKey})
	}

	return keys
}

func validateAccessDevice(d AccessDevice) error {
	if d.State != DevicePending && d.State != DeviceActive && d.State != DeviceRevoked {
		return fmt.Errorf("device %q: invalid state", d.ID)
	}

	if err := validateAccessLease(d); err != nil {
		return err
	}

	if d.State == DeviceActive || (d.State == DevicePending && d.TemporaryAddress == "") ||
		d.SSHPublicKey != "" || d.WGPublicKey != "" {
		if _, err := parseAccessSSHKey(d.SSHPublicKey); err != nil {
			return fmt.Errorf("device %q: %w", d.ID, err)
		}

		if !ValidAccessWGKey(d.WGPublicKey) {
			return fmt.Errorf("device %q: invalid WireGuard public key", d.ID)
		}
	}

	addr, err := netip.ParseAddr(d.Address)
	if err != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() {
		return fmt.Errorf("device %q: invalid IPv4 address", d.ID)
	}

	if !unixLoginPattern.MatchString(d.UnixLogin) {
		return fmt.Errorf("device %q: invalid Unix login", d.ID)
	}

	if d.UnixUID != "" {
		uid, err := strconv.ParseUint(d.UnixUID, 10, 32)
		if err != nil || uid == 0 || strconv.FormatUint(uid, 10) != d.UnixUID {
			return fmt.Errorf("device %q: invalid Unix UID", d.ID)
		}
	}

	digest, err := hex.DecodeString(d.RequestDigest)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != d.RequestDigest {
		return fmt.Errorf("device %q: invalid SHA-256 request digest", d.ID)
	}

	return nil
}

func validateAccessLease(d AccessDevice) error {
	if d.TemporaryAddress == "" && d.TemporaryPublicKey == "" && d.ExpiresAt.IsZero() {
		return nil
	}

	temporary, err := netip.ParseAddr(d.TemporaryAddress)
	if err != nil || !temporary.Is4() || temporary.IsUnspecified() || temporary.IsMulticast() ||
		d.TemporaryAddress == d.Address || !ValidAccessWGKey(d.TemporaryPublicKey) || (d.ExpiresAt.IsZero() != d.Recovery) {
		return fmt.Errorf("device %q: invalid enrollment lease", d.ID)
	}

	return nil
}

// ValidAccessWGKey accepts a canonical nonzero WireGuard public key.
func ValidAccessWGKey(key string) bool {
	wg, err := base64.StdEncoding.Strict().DecodeString(key)
	return err == nil && len(wg) == 32 && base64.StdEncoding.EncodeToString(wg) == key && !bytes.Equal(wg, make([]byte, 32))
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
