//go:build e2etest

package release

import (
	"bytes"
	"context"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
)

func currentBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("ERU_TEST_RELEASE_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return buildinfo.ReleaseBaseURL
}

func newDefaultVerifier() Verifier {
	return keyVerifier{}
}

type keyVerifier struct{}

func (keyVerifier) VerifyBlob(ctx context.Context, content, bundleBytes []byte, identity string) error {
	_ = ctx
	_ = identity
	path := os.Getenv("ERU_TEST_RELEASE_PUBKEY")
	if strings.TrimSpace(path) == "" {
		return verificationFailed("test release public key is not set")
	}
	pem, err := os.ReadFile(path) //nolint:gosec // G304: test-only path from ERU_TEST_RELEASE_PUBKEY.
	if err != nil {
		return verificationWrap(err)
	}
	pub, err := cryptoutils.UnmarshalPEMToPublicKey(pem)
	if err != nil {
		return verificationWrap(err)
	}
	sigVerifier, err := signature.LoadDefaultVerifier(pub)
	if err != nil {
		return verificationWrap(err)
	}
	material := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) {
		return root.NewExpiringKey(sigVerifier, time.Time{}, time.Time{}), nil
	})
	inner, err := verify.NewVerifier(material, verify.WithNoObserverTimestamps())
	if err != nil {
		return verificationWrap(err)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleBytes); err != nil {
		return verificationFailed("sigstore bundle is invalid")
	}
	policy := verify.NewPolicy(verify.WithArtifact(bytes.NewReader(content)), verify.WithKey())
	if _, err := inner.Verify(&b, policy); err != nil {
		return verificationWrap(err)
	}
	return nil
}
