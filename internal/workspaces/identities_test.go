package workspaces

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIdentityStorage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	original := &Workspace{Name: "prod", Server: "prod", Operator: Operator{Name: "original"}}
	createTestIdentities(t, original)
	checkIdentityFiles(t)

	if IdentityPath("prod", "", SSHKeyFile) != Path("prod", SSHKeyFile) {
		t.Fatal("original identity does not use legacy path")
	}

	if got, err := LoadIdentity("prod", ""); err != nil || got.Name != original.Name || got.Operator != original.Operator {
		t.Fatalf("original = %+v, %v", got, err)
	}

	if creds, err := LoadCredentials("prod"); err != nil || creds.SudoPassword != "original" {
		t.Fatalf("original credentials = %+v, %v", creds, err)
	}

	if names, err := List(); err != nil || !reflect.DeepEqual(names, []string{"prod"}) {
		t.Fatalf("workspace list = %v, %v", names, err)
	}

	if names, err := ListIdentities("prod"); err != nil || !reflect.DeepEqual(names, []string{"", "desktop", "laptop"}) {
		t.Fatalf("identities = %v, %v", names, err)
	}

	if err := RemoveIdentity("prod", "laptop"); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadIdentity("prod", "laptop"); !os.IsNotExist(err) {
		t.Fatalf("removed identity: %v", err)
	}

	if _, err := LoadIdentity("prod", "desktop"); err != nil {
		t.Fatal(err)
	}

	if err := RemoveIdentity("prod", ""); err == nil || !Exists("prod") {
		t.Fatalf("removed original identity: %v", err)
	}
}

func createTestIdentities(t *testing.T, original *Workspace) {
	t.Helper()

	if err := Save(original); err != nil {
		t.Fatal(err)
	}

	if err := SaveCredentials("prod", Credentials{SudoPassword: "original"}); err != nil {
		t.Fatal(err)
	}

	for _, identity := range []string{"laptop", "desktop"} {
		c := *original
		c.Operator.Name = identity

		if err := SaveIdentity(identity, &c); err != nil {
			t.Fatal(err)
		}

		for _, file := range []string{SSHKeyFile, WGKeyFile, KnownHostsFile, "tunnel.lock", "tunnel.sock"} {
			if err := WriteIdentityFile("prod", identity, file, []byte(identity)); err != nil {
				t.Fatal(err)
			}
		}

		if err := SaveIdentityCredentials("prod", identity, Credentials{SudoPassword: identity}); err != nil {
			t.Fatal(err)
		}
	}
}

func checkIdentityFiles(t *testing.T) {
	t.Helper()

	for _, identity := range []string{"laptop", "desktop"} {
		c, err := LoadIdentity("prod", identity)
		if err != nil || c.Name != "prod" || c.Operator.Name != identity {
			t.Fatalf("LoadIdentity(%q) = %+v, %v", identity, c, err)
		}

		creds, err := LoadIdentityCredentials("prod", identity)
		if err != nil || creds.SudoPassword != identity {
			t.Fatalf("credentials %q = %+v, %v", identity, creds, err)
		}

		for _, file := range []string{SSHKeyFile, WGKeyFile, KnownHostsFile, "tunnel.lock", "tunnel.sock"} {
			raw, err := ReadIdentityFile("prod", identity, file)
			if err != nil || string(raw) != identity {
				t.Fatalf("%s/%s = %q, %v", identity, file, raw, err)
			}
		}

		checkIdentityModes(t, identity)
	}
}

func checkIdentityModes(t *testing.T, identity string) {
	t.Helper()

	for _, path := range []string{filepath.Join(Dir("prod"), "identities"), IdentityDir("prod", identity)} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode %s: %v, %v", path, info, err)
		}
	}

	info, err := os.Stat(IdentityPath("prod", identity, CredentialsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode: %v, %v", info, err)
	}
}

func TestActiveIdentityAndPinnedProcess(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	createActiveTestIdentities(t)

	if err := SetActiveIdentity("prod", "a"); err != nil {
		t.Fatal(err)
	}

	checkActiveIdentity(t, "a")

	env, err := IdentityEnv("prod", "a")
	if err != nil || len(env) != 1 {
		t.Fatalf("pin env = %v, %v", env, err)
	}

	t.Setenv(EnvIdentityPin, strings.TrimPrefix(env[0], EnvIdentityPin+"="))

	if err := SetActiveIdentity("prod", "b"); err != nil {
		t.Fatal(err)
	}

	checkActiveIdentity(t, "a") // a daemon inherited the pin before another terminal switched.
	checkPinnedIdentityWrites(t)

	if err := os.Unsetenv(EnvIdentityPin); err != nil {
		t.Fatal(err)
	}

	checkActiveIdentity(t, "b")

	if err := SetActiveIdentity("prod", ""); err != nil {
		t.Fatal(err)
	}

	if c, err := Load("prod"); err != nil || c.Operator.Name != "original" {
		t.Fatalf("selected original = %+v, %v", c, err)
	}

	if err := PinIdentity("prod", ""); err != nil {
		t.Fatal(err)
	}

	if err := SetActiveIdentity("prod", "b"); err != nil {
		t.Fatal(err)
	}

	if c, err := Load("prod"); err != nil || c.Operator.Name != "original" {
		t.Fatalf("pinned original = %+v, %v", c, err)
	}
}

func createActiveTestIdentities(t *testing.T) {
	t.Helper()

	if err := Save(&Workspace{Name: "prod", Operator: Operator{Name: "original"}}); err != nil {
		t.Fatal(err)
	}

	if err := SaveCredentials("prod", Credentials{SudoPassword: "original"}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"a", "b"} {
		if err := SaveIdentity(id, &Workspace{Name: "prod", Operator: Operator{Name: id}}); err != nil {
			t.Fatal(err)
		}

		if err := SaveIdentityCredentials("prod", id, Credentials{SudoPassword: id}); err != nil {
			t.Fatal(err)
		}
	}
}

func checkActiveIdentity(t *testing.T, want string) {
	t.Helper()

	id, err := ActiveIdentity("prod")
	if err != nil || id != want {
		t.Fatalf("active identity = %q, %v; want %q", id, err, want)
	}

	c, err := Load("prod")
	if err != nil || c.Operator.Name != want {
		t.Fatalf("Load = %+v, %v; want %s", c, err, want)
	}

	creds, err := LoadCredentials("prod")
	if err != nil || creds.SudoPassword != want {
		t.Fatalf("credentials = %+v, %v; want %s", creds, err, want)
	}

	if Path("prod", SSHKeyFile) != IdentityPath("prod", want, SSHKeyFile) {
		t.Fatalf("wrong key path for %q", want)
	}
}

func checkPinnedIdentityWrites(t *testing.T) {
	t.Helper()

	c, err := Load("prod")
	if err != nil {
		t.Fatal(err)
	}

	c.Operator.WGAddress = "pinned"
	if err := Save(c); err != nil {
		t.Fatal(err)
	}

	if err := SaveCredentials("prod", Credentials{SudoPassword: "pinned"}); err != nil {
		t.Fatal(err)
	}

	if c, err := LoadIdentity("prod", "b"); err != nil || c.Operator.WGAddress != "" {
		t.Fatalf("save changed b: %+v, %v", c, err)
	}

	if creds, err := LoadIdentityCredentials("prod", "b"); err != nil || creds.SudoPassword != "b" {
		t.Fatalf("credentials changed b: %+v, %v", creds, err)
	}

	if c, err := LoadIdentity("prod", ""); err != nil || c.Operator.Name != "original" {
		t.Fatalf("explicit original = %+v, %v", c, err)
	}
}

func TestInvalidIdentitySelectorFailsClosed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := Save(&Workspace{Name: "prod"}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(Dir("prod"), identitySelectionFile), []byte(`"../escape"`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load("prod"); err == nil {
		t.Fatal("Load accepted invalid selector")
	}

	if _, err := LoadCredentials("prod"); err == nil {
		t.Fatal("LoadCredentials accepted invalid selector")
	}

	if _, err := os.Stat(Path("prod", SSHKeyFile)); err == nil || os.IsNotExist(err) {
		t.Fatalf("invalid selector did not return unusable path: %v", err)
	}

	if _, err := LoadIdentity("prod", ""); err != nil {
		t.Fatalf("explicit original must ignore selector: %v", err)
	}
}

func TestIdentityValidationAndNoFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := Save(&Workspace{Name: "prod"}); err != nil {
		t.Fatal(err)
	}

	if err := SaveCredentials("prod", Credentials{SudoPassword: "original"}); err != nil {
		t.Fatal(err)
	}

	if got, err := LoadIdentityCredentials("prod", "missing"); err != nil || got.SudoPassword != "" {
		t.Fatalf("missing identity credentials fell back: %+v, %v", got, err)
	}

	if _, err := ReadIdentityFile("prod", "missing", SSHKeyFile); !os.IsNotExist(err) {
		t.Fatalf("missing key: %v", err)
	}

	for _, identity := range []string{"../escape", ".", "..", "UPPER", "foo/bar"} {
		if err := SaveIdentity(identity, &Workspace{Name: "prod"}); err == nil {
			t.Fatalf("accepted identity %q", identity)
		}

		if err := WriteIdentityFile("prod", identity, SSHKeyFile, nil); err == nil {
			t.Fatalf("accepted identity path %q", identity)
		}
	}

	if err := SaveIdentity("device", &Workspace{Name: "missing"}); err == nil {
		t.Fatal("created identity without canonical workspace")
	}
}
