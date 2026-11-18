package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/buildinfo"
)

const edgeManifestVersion = "edge-0123456789ab"

func subsetManifest(m Manifest, keys ...string) Manifest {
	out := m

	out.Artifacts = make(map[string]Artifact, len(keys))
	for _, key := range keys {
		out.Artifacts[key] = m.Artifacts[key]
	}

	return out
}

func TestValidateEdgeArtifactSubset(t *testing.T) {
	full := testManifest(ChannelEdge, edgeManifestVersion)
	base := versionRoot(edgeManifestVersion)
	unknown := subsetManifest(full, "linux/amd64")
	unknown.Artifacts["linux/386"] = Artifact{URL: base + "/linux/386/vaire", SHA256: strings.Repeat("a", 64)}
	traversal := subsetManifest(full, "linux/amd64")
	traversal.Artifacts["../linux/amd64"] = Artifact{URL: base + "/../linux/amd64/vaire", SHA256: strings.Repeat("a", 64)}
	badURL := subsetManifest(full, "linux/amd64")
	badURL.Artifacts["linux/amd64"] = Artifact{URL: base + "/linux/arm64/vaire", SHA256: strings.Repeat("a", 64)}
	badSHA := subsetManifest(full, "linux/amd64")
	badSHA.Artifacts["linux/amd64"] = Artifact{URL: base + "/linux/amd64/vaire", SHA256: strings.Repeat("A", 64)}
	nilArtifacts := full
	nilArtifacts.Artifacts = nil
	empty := full
	empty.Artifacts = map[string]Artifact{}

	cases := []struct {
		name string
		m    Manifest
		ok   bool
	}{
		{"one platform", subsetManifest(full, "linux/amd64"), true},
		{"mixed subset", subsetManifest(full, "linux/amd64", "darwin/arm64"), true},
		{"all four", full, true},
		{"nil artifacts", nilArtifacts, false},
		{"empty artifacts", empty, false},
		{"unknown platform", unknown, false},
		{"path traversal platform", traversal, false},
		{"artifact URL mismatch", badURL, false},
		{"artifact SHA invalid", badSHA, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.m, ChannelEdge, "")
			if (err == nil) != c.ok {
				t.Fatalf("Validate = %v, want ok %v", err, c.ok)
			}
		})
	}
}

func TestValidateTaggedRequiresEveryArtifact(t *testing.T) {
	for _, c := range []struct {
		channel string
		signing string
		version string
	}{
		{ChannelStable, ChannelStable, "1.2.3"},
		{"stable-1.x", ChannelStable, "1.2.3"},
		{ChannelRC, ChannelRC, "1.2.3-rc.1"},
	} {
		m := testManifest(c.signing, c.version)
		if err := Validate(m, c.channel, ""); err != nil {
			t.Fatalf("Validate full %s manifest: %v", c.channel, err)
		}

		subset := subsetManifest(m, "linux/amd64")
		if err := Validate(subset, c.channel, ""); err == nil {
			t.Errorf("Validate(%s) accepted a platform subset", c.channel)
		}
	}
}

func TestValidateNamedEdgeManifest(t *testing.T) {
	for _, version := range []string{"1.0.0-alpha.1", "1.0.0-beta.4"} {
		m := subsetManifest(testManifest(ChannelEdge, version), "linux/amd64")
		if err := Validate(m, ChannelEdge, version); err != nil {
			t.Errorf("Validate(%s, %s): %v", ChannelEdge, version, err)
		}
	}

	m := testManifest(ChannelRC, "1.0.0-rc.1")
	if err := Validate(m, ChannelEdge, ""); err == nil {
		t.Fatal("edge accepted an rc manifest")
	}

	if err := Validate(m, ChannelRC, ""); err != nil {
		t.Fatalf("Validate rc manifest: %v", err)
	}

	if err := Validate(m, ChannelStable, ""); err == nil {
		t.Fatal("stable accepted an rc manifest")
	}
}

func TestClientResolveUsesVersionSigningIdentity(t *testing.T) {
	base := strings.TrimRight(buildinfo.ReleaseBaseURL, "/")

	for _, c := range []struct {
		channel string
		version string
		root    string
	}{
		{ChannelEdge, "1.0.0-beta.4", "/edge"},
		{ChannelRC, "1.0.0-rc.2", "/rc"},
	} {
		manifest := testManifest(c.channel, c.version)

		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}

		manifestURL := base + c.root + "/manifest.json"
		bundle := []byte(`{"fake":"sigstore bundle"}`)
		transport := &fakeRoundTripper{
			routes: map[string][]byte{
				manifestURL:                    raw,
				manifestURL + ".sigstore.json": bundle,
			},
			requests: map[string]int{},
		}
		verifier := &fakeVerifier{}
		client := &Client{HTTP: &http.Client{Transport: transport}, Verifier: verifier}

		got, err := client.Resolve(context.Background(), c.channel, "")
		if err != nil {
			t.Fatalf("Resolve %s: %v", c.channel, err)
		}

		want, err := Identity(c.channel, c.version)
		if err != nil {
			t.Fatal(err)
		}

		if got.Version != c.version {
			t.Errorf("Resolve %s version = %q, want %q", c.channel, got.Version, c.version)
		}

		if len(verifier.calls) != 1 {
			t.Fatalf("Resolve %s verifier calls = %d, want 1", c.channel, len(verifier.calls))
		}

		if verifier.calls[0].identity != want {
			t.Errorf("Resolve %s identity = %q, want %q", c.channel, verifier.calls[0].identity, want)
		}
	}
}

func TestClientRejectsChannelVersionMismatch(t *testing.T) {
	client := &Client{}

	for _, c := range []struct{ channel, version string }{
		{ChannelEdge, "1.0.0-rc.1"},
		{ChannelRC, "1.0.0"},
		{ChannelRC, "1.0.0-beta.1"},
		{ChannelStable, "1.0.0-alpha.1"},
		{"stable-1.x", "1.0.0-beta.1"},
		{ChannelEdge, "1.0.0"},
	} {
		if _, err := client.Resolve(context.Background(), c.channel, c.version); err == nil {
			t.Errorf("Resolve(%q, %q) accepted a mismatched channel and version", c.channel, c.version)
		}
	}
}

type fakeRoundTripper struct {
	routes   map[string][]byte
	requests map[string]int
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rawURL := req.URL.String()
	f.requests[rawURL]++

	body, ok := f.routes[rawURL]
	if !ok {
		return nil, fmt.Errorf("unexpected release request for %s", rawURL)
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

type verifyCall struct {
	content  []byte
	bundle   []byte
	identity string
}

type fakeVerifier struct {
	calls []verifyCall
	err   error
}

func (f *fakeVerifier) VerifyBlob(_ context.Context, content, bundle []byte, identity string) error {
	f.calls = append(f.calls, verifyCall{
		content:  append([]byte(nil), content...),
		bundle:   append([]byte(nil), bundle...),
		identity: identity,
	})

	return f.err
}

func TestClientResolveAndFetchEdge(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	binary := []byte("fake edge vaire binary")
	sum := sha256.Sum256(binary)
	manifest := subsetManifest(testManifest(ChannelEdge, edgeManifestVersion), "linux/amd64")
	artifactURL := versionRoot(edgeManifestVersion) + "/linux/amd64/vaire"
	manifest.Artifacts["linux/amd64"] = Artifact{URL: artifactURL, SHA256: hex.EncodeToString(sum[:])}

	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	bundle := []byte(`{"fake":"sigstore bundle"}`)

	base := strings.TrimRight(buildinfo.ReleaseBaseURL, "/")
	channelURL := base + "/edge/manifest.json"
	versionURL := base + "/releases/" + edgeManifestVersion + "/manifest.json"
	transport := &fakeRoundTripper{
		routes: map[string][]byte{
			channelURL:                    raw,
			channelURL + ".sigstore.json": bundle,
			versionURL:                    raw,
			versionURL + ".sigstore.json": bundle,
			artifactURL:                   binary,
		},
		requests: map[string]int{},
	}
	verifier := &fakeVerifier{}
	client := &Client{HTTP: &http.Client{Transport: transport}, Verifier: verifier}
	ctx := context.Background()

	channelManifest, err := client.Resolve(ctx, ChannelEdge, "")
	if err != nil {
		t.Fatalf("Resolve edge channel: %v", err)
	}

	if channelManifest.Version != edgeManifestVersion {
		t.Fatalf("resolved version %q, want %q", channelManifest.Version, edgeManifestVersion)
	}

	pinned, err := client.Resolve(ctx, ChannelEdge, edgeManifestVersion)
	if err != nil {
		t.Fatalf("Resolve pinned edge version: %v", err)
	}

	if pinned.Version != edgeManifestVersion {
		t.Fatalf("pinned version %q, want %q", pinned.Version, edgeManifestVersion)
	}

	wantIdentity, err := Identity(ChannelEdge, edgeManifestVersion)
	if err != nil {
		t.Fatal(err)
	}

	if len(verifier.calls) != 2 {
		t.Fatalf("verifier calls = %d, want 2", len(verifier.calls))
	}

	for i, call := range verifier.calls {
		if call.identity != wantIdentity {
			t.Errorf("call %d identity = %q, want %q", i, call.identity, wantIdentity)
		}

		if !bytes.Equal(call.content, raw) || !bytes.Equal(call.bundle, bundle) {
			t.Errorf("call %d verified different content or bundle", i)
		}
	}

	path, err := client.Fetch(ctx, channelManifest, "linux", "amd64")
	if err != nil {
		t.Fatalf("Fetch linux/amd64: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, binary) {
		t.Fatalf("fetched bytes = %q, want %q", got, binary)
	}

	if transport.requests[artifactURL] != 1 {
		t.Fatalf("artifact requests = %d, want 1", transport.requests[artifactURL])
	}

	requests := len(transport.requests)

	if _, err := client.Fetch(ctx, pinned, "linux", "arm64"); err == nil {
		t.Fatal("Fetch linux/arm64 succeeded for a manifest without it")
	}

	if len(transport.requests) != requests {
		t.Fatal("Fetch linux/arm64 attempted a download")
	}

	verifier.err = fmt.Errorf("untrusted signature")
	if _, err := client.Resolve(ctx, ChannelEdge, edgeManifestVersion); err == nil {
		t.Fatal("Resolve ignored a verifier error")
	}
}
