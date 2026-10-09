//go:build !e2etest

package release

import (
	"bytes"
	"context"
	"sync"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

func currentBaseURL() string {
	return buildinfo.ReleaseBaseURL
}

func newDefaultVerifier() Verifier {
	return &prodVerifier{}
}

type prodVerifier struct {
	once    sync.Once
	inner   *verify.Verifier
	initErr error
}

func (v *prodVerifier) VerifyBlob(ctx context.Context, content, bundleBytes []byte, identity string) error {
	_ = ctx
	v.once.Do(v.init)
	if v.initErr != nil {
		return v.initErr
	}
	certID, err := verify.NewShortCertificateIdentity(Issuer, "", identity, "")
	if err != nil {
		return verificationWrap(err)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleBytes); err != nil {
		return verificationFailed("sigstore bundle is invalid")
	}
	policy := verify.NewPolicy(verify.WithArtifact(bytes.NewReader(content)), verify.WithCertificateIdentity(certID))
	if _, err := v.inner.Verify(&b, policy); err != nil {
		return verificationWrap(err)
	}
	return nil
}

func (v *prodVerifier) init() {
	opts := tuf.DefaultOptions()
	opts.WithCachePath(tufCacheDir())
	client, err := tuf.New(opts)
	if err != nil {
		v.initErr = unreachable(tuf.DefaultMirror, err)
		return
	}
	trusted, err := root.GetTrustedRoot(client)
	if err != nil {
		v.initErr = unreachable(tuf.DefaultMirror, err)
		return
	}
	inner, err := verify.NewVerifier(trusted, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		v.initErr = verificationWrap(err)
		return
	}
	v.inner = inner
}
