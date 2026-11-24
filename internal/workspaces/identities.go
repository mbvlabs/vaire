package workspaces

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/pelletier/go-toml/v2"
)

// EnvIdentityPin pins one workspace's identity for this process and children.
const EnvIdentityPin = "VAIRE_IDENTITY_PIN"

const identitySelectionFile = "current_identity.json"

type identityPin struct {
	Workspace string `json:"workspace"`
	Identity  string `json:"identity"`
}

// IdentityEnv returns an environment entry that pins a workspace's identity.
// Replace any existing EnvIdentityPin entry when constructing a child env.
func IdentityEnv(name, identity string) ([]string, error) {
	if err := validateIdentityTarget(name, identity); err != nil {
		return nil, err
	}

	raw, err := json.Marshal(identityPin{Workspace: name, Identity: identity})
	if err != nil {
		return nil, err
	}

	return []string{EnvIdentityPin + "=" + string(raw)}, nil
}

// PinIdentity fixes the identity for this process and its children, even if
// the persistent selector is changed elsewhere. It does not change the saved
// selector. Call before starting concurrent workspace operations.
func PinIdentity(name, identity string) error {
	if _, err := LoadIdentity(name, identity); err != nil {
		return err
	}

	env, err := IdentityEnv(name, identity)
	if err != nil {
		return err
	}

	return os.Setenv(EnvIdentityPin, env[0][len(EnvIdentityPin)+1:])
}

// ActiveIdentity returns a process pin, if present for name, or its saved
// selector. Missing selectors choose the original identity.
func ActiveIdentity(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}

	if raw, ok := os.LookupEnv(EnvIdentityPin); ok {
		var pin identityPin
		if err := json.Unmarshal([]byte(raw), &pin); err != nil {
			return "", fmt.Errorf("invalid %s: %w", EnvIdentityPin, err)
		}

		if err := validateIdentityTarget(pin.Workspace, pin.Identity); err != nil {
			return "", fmt.Errorf("invalid %s: %w", EnvIdentityPin, err)
		}

		if pin.Workspace == name {
			return pin.Identity, nil
		}
	}

	raw, err := os.ReadFile(filepath.Join(Dir(name), identitySelectionFile))
	if os.IsNotExist(err) {
		return "", nil
	}

	if err != nil {
		return "", err
	}

	var identity string
	if err := json.Unmarshal(raw, &identity); err != nil {
		return "", fmt.Errorf("invalid identity selector for workspace %s: %w", name, err)
	}

	if err := ValidateIdentity(identity); err != nil {
		return "", fmt.Errorf("invalid identity selector for workspace %s: %w", name, err)
	}

	return identity, nil
}

// SetActiveIdentity changes the persistent selector, not a process pin.
func SetActiveIdentity(name, identity string) error {
	if err := validateIdentityTarget(name, identity); err != nil {
		return err
	}

	if _, err := LoadIdentity(name, identity); err != nil {
		return err
	}

	raw, err := json.Marshal(identity)
	if err != nil {
		return err
	}

	return transport.WriteSecretFile(filepath.Join(Dir(name), identitySelectionFile), append(raw, '\n'))
}

// ValidateIdentity checks a local identity name. The empty name selects the
// original identity stored directly in the workspace directory.
func ValidateIdentity(identity string) error {
	if identity == "" || NamePattern.MatchString(identity) {
		return nil
	}

	return output.NewError(output.CodeValidation, "identity name "+identity+" is invalid", output.ExitConfig,
		"Use lower-case letters, digits, '.', '_' and '-'.")
}

// IdentityDir returns the directory for an identity. Callers must validate
// both names before using paths derived from untrusted input.
func IdentityDir(name, identity string) string {
	if identity == "" {
		return Dir(name)
	}

	return filepath.Join(Dir(name), "identities", identity)
}

// IdentityPath locates an identity's manifest, keys, credentials or tunnel
// files. The empty identity preserves the original workspace layout.
func IdentityPath(name, identity, file string) string {
	return filepath.Join(IdentityDir(name, identity), file)
}

func validateIdentityTarget(name, identity string) error {
	if err := ValidateName(name); err != nil {
		return err
	}

	return ValidateIdentity(identity)
}

// LoadIdentity reads an identity's manifest. Its Name remains the canonical
// workspace name, never the identity name.
func LoadIdentity(name, identity string) (*Workspace, error) {
	if err := validateIdentityTarget(name, identity); err != nil {
		return nil, err
	}

	if !Exists(name) {
		return nil, notFound(name)
	}

	path := IdentityPath(name, identity, WorkspaceFile)

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var c Workspace
	if err := toml.Unmarshal(raw, &c); err != nil {
		return nil, output.NewError(output.CodeConfig, path+" does not parse: "+err.Error(), output.ExitConfig,
			"Recreate the identity with vaire login.")
	}

	c.Name = name

	return &c, nil
}

// SaveIdentity writes an identity's manifest without modifying the original.
func SaveIdentity(identity string, c *Workspace) error {
	if err := validateIdentityTarget(c.Name, identity); err != nil {
		return err
	}

	if identity != "" && !Exists(c.Name) {
		return notFound(c.Name)
	}

	if err := ensureRoot(); err != nil {
		return err
	}

	return saveWorkspaceFile(IdentityPath(c.Name, identity, WorkspaceFile), c)
}

// WriteIdentityFile writes a private file for an identity.
func WriteIdentityFile(name, identity, file string, data []byte) error {
	if err := validateIdentityTarget(name, identity); err != nil {
		return err
	}

	if identity == "" {
		if err := ensureRoot(); err != nil {
			return err
		}

		return transport.WriteSecretFile(IdentityPath(name, "", file), data)
	}

	if !Exists(name) {
		return notFound(name)
	}

	return transport.WriteSecretFile(IdentityPath(name, identity, file), data)
}

// ReadIdentityFile reads a file belonging to one identity, without falling
// back to another identity if it is missing.
func ReadIdentityFile(name, identity, file string) ([]byte, error) {
	if err := validateIdentityTarget(name, identity); err != nil {
		return nil, err
	}

	return os.ReadFile(IdentityPath(name, identity, file))
}

// SaveIdentityCredentials writes one identity's credentials.
func SaveIdentityCredentials(name, identity string, creds Credentials) error {
	raw, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}

	return WriteIdentityFile(name, identity, CredentialsFile, append(raw, '\n'))
}

// LoadIdentityCredentials reads credentials without falling back to the
// original identity. A missing credentials file means empty credentials.
func LoadIdentityCredentials(name, identity string) (Credentials, error) {
	raw, err := ReadIdentityFile(name, identity, CredentialsFile)
	if os.IsNotExist(err) {
		return Credentials{}, nil
	}

	if err != nil {
		return Credentials{}, err
	}

	var creds Credentials

	err = json.Unmarshal(raw, &creds)

	return creds, err
}

// ListIdentities lists published named identities; the original identity is
// represented by the empty string and is included when the workspace exists.
func ListIdentities(name string) ([]string, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	if !Exists(name) {
		return nil, notFound(name)
	}

	names := []string{""}

	entries, err := os.ReadDir(filepath.Join(Dir(name), "identities"))
	if os.IsNotExist(err) {
		return names, nil
	}

	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() && NamePattern.MatchString(entry.Name()) {
			if _, err := os.Stat(IdentityPath(name, entry.Name(), WorkspaceFile)); err == nil {
				names = append(names, entry.Name())
			}
		}
	}

	sort.Strings(names)

	return names, nil
}

// RemoveIdentity removes a named identity, never the original workspace.
func RemoveIdentity(name, identity string) error {
	if err := validateIdentityTarget(name, identity); err != nil {
		return err
	}

	if identity == "" {
		return output.NewError(output.CodeValidation, "cannot remove the original identity", output.ExitConfig,
			"Remove the workspace to remove its original identity.")
	}

	if _, err := LoadIdentity(name, identity); err != nil {
		return err
	}

	return os.RemoveAll(IdentityDir(name, identity))
}
