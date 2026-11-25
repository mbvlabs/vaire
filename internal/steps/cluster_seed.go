package steps

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/secrets"
)

// The encrypted master bundle carries these verifiers, never runtime passwords.
var clusterSeedDir = RuntimeDir

const clusterSeedName = "cluster-verifiers.json"

type clusterSeed struct {
	Admin   string `json:"admin"`
	Auth    string `json:"pgbouncer_auth"`
	Monitor string `json:"monitor"`
}

// seedFile reads only a private regular file owned by the node process. The
// runtime directory itself is checked separately and must not be replaceable
// by service users. In production the node process runs as root.
func seedFile(name string) ([]byte, error) {
	f, err := os.OpenFile(filepath.Join(clusterSeedDir, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(st.Uid) != int64(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, errors.New("unsafe cluster credential file")
	}

	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return nil, errors.New("invalid cluster credential file")
	}

	return data, nil
}

func checkSeedDir() error {
	info, err := os.Lstat(clusterSeedDir)
	if err != nil {
		return err
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(st.Uid) != int64(os.Geteuid()) || !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&os.ModeType != os.ModeDir {
		return errors.New("unsafe cluster credential directory")
	}

	return nil
}

func readClusterSeed() (clusterSeed, error) {
	var seed clusterSeed
	if err := checkSeedDir(); err != nil {
		return seed, err
	}

	raw, err := seedFile(clusterSeedName)
	if err != nil {
		return seed, err
	}

	return decodeClusterSeed(raw)
}

// ValidateClusterSeed ensures a bundled seed contains only SCRAM verifiers.
func ValidateClusterSeed(raw []byte) error {
	_, err := decodeClusterSeed(raw)
	return err
}

// VerifyClusterSeed checks recovery passwords without writing node state.
func VerifyClusterSeed(raw []byte, values map[string]string) error {
	seed, err := decodeClusterSeed(raw)
	if err != nil {
		return err
	}

	return verifySeedPasswords(seed, values)
}

func decodeClusterSeed(raw []byte) (clusterSeed, error) {
	var seed clusterSeed

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&seed); err != nil {
		return clusterSeed{}, err
	}

	if dec.Decode(&struct{}{}) != io.EOF || !pg.ValidSCRAMVerifier(seed.Admin) || !pg.ValidSCRAMVerifier(seed.Auth) || !pg.ValidSCRAMVerifier(seed.Monitor) {
		return clusterSeed{}, errors.New("invalid cluster credential seed")
	}

	for _, v := range []string{seed.Admin, seed.Auth, seed.Monitor} {
		secrets.Add(v)
	}

	return seed, nil
}

// CheckClusterSeed refuses missing, unsafe, or inconsistent node credentials
// before a member's create can write any topology or provision a cluster.
func CheckClusterSeed() error {
	seed, err := readClusterSeed()
	if err != nil {
		return err
	}

	for _, item := range []struct{ name, verifier string }{{"pgbouncer_auth.pass", seed.Auth}, {"monitor.pass", seed.Monitor}} {
		raw, err := seedFile(item.name)
		if err != nil || !strings.HasSuffix(string(raw), "\n") || !pg.VerifySCRAM(item.verifier, strings.TrimSuffix(string(raw), "\n")) {
			return errors.New("missing or inconsistent cluster service credential")
		}

		secrets.Add(strings.TrimSuffix(string(raw), "\n"))
	}

	return nil
}

// SeedClusterCredentials is called only for an owned fresh bootstrap.
// It never replaces existing credentials, including during a setup retry.
func SeedClusterCredentials(values map[string]string) error {
	keys := []string{SecretAdminPassword, SecretPgbouncerAuth, SecretMonitor}
	for _, key := range keys {
		if values[key] == "" || strings.ContainsAny(values[key], "\r\n") {
			return errors.New("cluster credentials are missing or invalid")
		}

		secrets.Add(values[key])
	}

	if err := os.MkdirAll(clusterSeedDir, 0o700); err != nil {
		return err
	}

	if err := checkSeedDir(); err != nil {
		return err
	}

	seed, err := readClusterSeed()
	if err == nil {
		if !pg.VerifySCRAM(seed.Admin, values[keys[0]]) || !pg.VerifySCRAM(seed.Auth, values[keys[1]]) || !pg.VerifySCRAM(seed.Monitor, values[keys[2]]) {
			return errors.New("cluster credentials do not match the existing seed")
		}

		return CheckClusterSeed()
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if err := seedRuntimePasswords(values); err != nil {
		return err
	}

	seed.Admin, err = pg.SCRAMVerifier(values[keys[0]])
	if err != nil {
		return err
	}

	seed.Auth, err = pg.SCRAMVerifier(values[keys[1]])
	if err != nil {
		return err
	}

	seed.Monitor, err = pg.SCRAMVerifier(values[keys[2]])
	if err != nil {
		return err
	}

	raw, err := json.Marshal(seed)
	if err != nil {
		return err
	}

	return engine.WriteFileAtomic(filepath.Join(clusterSeedDir, clusterSeedName), append(raw, '\n'), 0o600)
}

// RestoreClusterCredentials validates every supplied password against the
// restored verifiers before writing any service password. Older bundles have
// no seed and keep the pre-existing restore behavior.
func RestoreClusterCredentials(values map[string]string) error {
	_, err := os.Lstat(filepath.Join(clusterSeedDir, clusterSeedName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	seed, err := readClusterSeed()
	if err != nil {
		return err
	}

	if err := verifySeedPasswords(seed, values); err != nil {
		return err
	}

	for _, key := range []string{SecretAdminPassword, SecretPgbouncerAuth, SecretMonitor} {
		secrets.Add(values[key])
	}

	if err := seedRuntimePasswords(values); err != nil {
		return err
	}

	return CheckClusterSeed()
}

func verifySeedPasswords(seed clusterSeed, values map[string]string) error {
	if values[SecretAdminPassword] == "" || values[SecretPgbouncerAuth] == "" || values[SecretMonitor] == "" ||
		!pg.VerifySCRAM(seed.Admin, values[SecretAdminPassword]) ||
		!pg.VerifySCRAM(seed.Auth, values[SecretPgbouncerAuth]) ||
		!pg.VerifySCRAM(seed.Monitor, values[SecretMonitor]) {
		return errors.New("recovery credentials do not match the cluster seed")
	}

	return nil
}

// Write runtime passwords first; a retry must match them before seeding.
func seedRuntimePasswords(values map[string]string) error {
	for _, item := range []struct{ name, password string }{
		{"pgbouncer_auth.pass", values[SecretPgbouncerAuth]}, {"monitor.pass", values[SecretMonitor]},
	} {
		raw, err := seedFile(item.name)
		if errors.Is(err, fs.ErrNotExist) {
			if err := engine.WriteFileAtomic(filepath.Join(clusterSeedDir, item.name), []byte(item.password+"\n"), 0o600); err != nil {
				return err
			}
		} else if err != nil || string(raw) != item.password+"\n" {
			return errors.New("existing cluster service credential does not match")
		}
	}

	return nil
}
