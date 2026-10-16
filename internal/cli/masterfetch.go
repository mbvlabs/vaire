package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/masterbackup"
)

// Operator-side master bundles (Phase 5): list, pick, download, check and
// open them with the recovery identity. Nothing is ever written in clear.

// fetchedBundle is a bundle as downloaded or read from a file, still
// encrypted.
type fetchedBundle struct {
	Name    string
	Cipher  []byte
	Sidecar *masterbackup.Sidecar
	// Older is the next older complete bundle, for a corrupt one's hint.
	Older string
	// From is "bucket" or the file it was read from.
	From string
}

// pickBundleName resolves --bundle: a name, a time (the newest at or
// before it), or empty for the newest of the highest epoch.
func pickBundleName(list []backup.MasterBundle, want string) (masterbackup.Parsed, string, error) {
	complete := map[string]bool{}
	for _, b := range list {
		complete[b.Name] = b.Complete
	}
	var before time.Time
	name := strings.TrimSuffix(strings.TrimSuffix(want, masterbackup.BundleSuffix), masterbackup.SidecarSuffix)
	if name != "" {
		if _, ok := masterbackup.ParseName(name); !ok {
			t, err := parseTarget(want, time.Now())
			if err != nil {
				return masterbackup.Parsed{}, "", output.NewError(output.CodeUsage, "--bundle "+want+" is neither a bundle name nor a time", output.ExitUsage,
					"Use a name from eru server backup list, or an RFC 3339 time.")
			}
			before, name = t, ""
		}
	}
	p, ok := masterbackup.Pick(complete, name, before)
	if !ok {
		return p, "", output.NewError(output.CodeBundleNotFound, "no complete master bundle matches", output.ExitPrecondition,
			"List the bundles with eru server backup list; without a bucket, restore from an export with --bundle-file.").
			WithDetails(map[string]any{"bundle": want})
	}
	older := ""
	for _, b := range list {
		if b.Complete && b.Name < p.Name && b.Epoch <= p.Epoch && (older == "" || b.Name > older) {
			older = b.Name
		}
	}
	return p, older, nil
}

// fetchBundleFromBucket lists <prefix>/master/ and downloads one bundle.
func fetchBundleFromBucket(ctx context.Context, kit *contexts.BackupCreds, want string) (fetchedBundle, error) {
	t := kitTarget(kit)
	list, err := backup.MasterList(ctx, t)
	if err != nil {
		return fetchedBundle{}, err
	}
	p, older, err := pickBundleName(list, want)
	if err != nil {
		return fetchedBundle{}, err
	}
	cipher, side, err := backup.MasterGet(ctx, t, p.Name)
	if err != nil {
		return fetchedBundle{}, err
	}
	return fetchedBundle{Name: p.Name, Cipher: cipher, Sidecar: &side, Older: older, From: "bucket"}, nil
}

// readBundleFile reads an exported bundle, - for stdin.
func readBundleFile(path string, stdin io.Reader) (fetchedBundle, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(io.LimitReader(stdin, 64<<20))
	} else {
		raw, err = os.ReadFile(path) //nolint:gosec // G304: the operator names the bundle file.
	}
	if err != nil {
		return fetchedBundle{}, output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
	}
	name := strings.TrimSuffix(baseName(path), masterbackup.BundleSuffix)
	if _, ok := masterbackup.ParseName(name); !ok {
		name = ""
	}
	return fetchedBundle{Name: name, Cipher: raw, From: path}, nil
}

func baseName(p string) string { return p[strings.LastIndex(p, "/")+1:] }

// fromFile reports whether the bundle was read from a file (or stdin)
// rather than the bucket.
func (f fetchedBundle) fromFile() bool { return f.From != "" && f.From != "bucket" }

// label names the bundle in messages: its name, else the file it was read
// from.
func (f fetchedBundle) label() string {
	switch {
	case f.Name != "":
		return f.Name
	case f.From == "-":
		return "on stdin"
	default:
		return f.From
	}
}

// openFetched checks the sidecar's ciphertext hash, decrypts with the
// recovery identity and validates the bundle. A failed check is
// bundle_corrupt naming the next older bundle.
func openFetched(f fetchedBundle, identity, server string) (masterbackup.Bundle, error) {
	corrupt := func(cause string) error {
		hint := "Pick another bundle with --bundle (eru server backup list)."
		switch {
		case f.fromFile():
			hint = "Use another exported file with --bundle-file, or a bundle from the bucket with --bundle (eru server backup list)."
		case f.Older != "":
			hint = "The next older complete bundle is " + f.Older + ": --bundle " + f.Older + "."
		}
		return output.NewError(output.CodeBundleCorrupt, "master bundle "+f.label()+" is corrupt: "+cause, output.ExitSecrets, hint).
			WithDetails(map[string]any{"bundle": f.Name, "from": f.From, "older": f.Older})
	}
	if f.Sidecar != nil && f.Sidecar.SHA256 != "" && f.Sidecar.SHA256 != masterbackup.SHA256(f.Cipher) {
		return masterbackup.Bundle{}, corrupt("its sha256 differs from its sidecar's")
	}
	b, err := masterbackup.Open(f.Cipher, identity)
	if errors.Is(err, masterbackup.ErrCorrupt) {
		// The cause without ErrCorrupt's own "the bundle is corrupt: ".
		return masterbackup.Bundle{}, corrupt(strings.TrimPrefix(err.Error(), masterbackup.ErrCorrupt.Error()+": "))
	}
	if err != nil {
		return masterbackup.Bundle{}, output.WrapError(output.CodeSecrets, err, output.ExitSecrets, "")
	}
	if server != "" && b.Manifest.Server != server {
		return b, output.NewError(output.CodePrecondition, "master bundle "+f.label()+" belongs to server "+b.Manifest.Server+", not "+server, output.ExitPrecondition,
			"Use the context of the server the bundle came from.").WithDetails(map[string]any{"reason": "bundle_server_mismatch", "bundle_server": b.Manifest.Server})
	}
	if newerThanBuild(b.Manifest.EruVersion) {
		return b, output.NewError(output.CodeUpdateRequired, "master bundle "+f.label()+" was written by eru "+b.Manifest.EruVersion+", newer than this "+buildinfo.Version,
			output.ExitVersion, "Run eru upgrade first.").WithDetails(map[string]any{"bundle_version": b.Manifest.EruVersion})
	}
	return b, nil
}

// newerThanBuild reports whether a bundle's eru version is a newer stable
// release than this build. Edge and dev builds aren't compared.
func newerThanBuild(v string) bool {
	a, okA := semverParts(v)
	b, okB := semverParts(buildinfo.Version)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func semverParts(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n := 0
		if p == "" {
			return out, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, false
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, true
}

// recoveryIdentity is the kit's age identity, which opens master bundles.
func recoveryIdentity(name string) (*contexts.BackupCreds, string, error) {
	creds, err := contexts.LoadCredentials(name)
	if err != nil {
		return nil, "", err
	}
	if creds.Backup == nil || creds.Backup.Dumps.AgeIdentity == "" {
		return creds.Backup, "", output.NewError(output.CodeInputRequired, "this context's recovery kit has no age identity", output.ExitInput,
			"Master bundles open with the recovery identity. eru login with a current secrets document (eru secrets export).").
			WithDetails(map[string]any{"reason": "recovery_kit_missing"})
	}
	return creds.Backup, creds.Backup.Dumps.AgeIdentity, nil
}
