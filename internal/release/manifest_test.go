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

func TestValidateStableRequiresEveryArtifact(t *testing.T) {
	m := testManifest(ChannelStable, "1.2.3")
	if err := Validate(m, ChannelStable, ""); err != nil {
		t.Fatalf("Validate full stable manifest: %v", err)
	}

	subset := subsetManifest(m, "linux/amd64")
	for _, channel := range []string{ChannelStable, "stable-1.x"} {
		if err := Validate(subset, channel, ""); err == nil {
			t.Errorf("Validate(%s) accepted a platform subset", channel)
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
