package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBucketPromotion(t *testing.T) {
	for _, key := range []string{"manifest.json", "1.x/manifest.json"} {
		for _, tc := range []struct {
			name, listing, manifest, candidate, want string
			listErr, readErr                         bool
		}{
			{name: "absent", listing: `{"KeyCount":0}`, candidate: "1.0.0-alpha.1", want: "yes\n"},
			{name: "only sidecar exists", listing: `{"KeyCount":1,"Contents":[{"Key":"` + key + `.sigstore.json"}]}`, candidate: "1.0.0", want: "yes\n"},
			{name: "older tag", manifest: `{"version":"1.1.0"}`, candidate: "1.0.1", want: "no\n"},
			{name: "next alpha", manifest: `{"version":"1.0.0-alpha.2"}`, candidate: "1.0.0-alpha.10", want: "yes\n"},
			{name: "alpha cannot replace final", manifest: `{"version":"1.1.0"}`, candidate: "1.2.0-alpha.1", want: "no\n"},
			{name: "next final", manifest: `{"version":"1.1.0"}`, candidate: "1.1.1", want: "yes\n"},
			{name: "list failure", listErr: true, candidate: "1.0.0"},
			{name: "read failure", readErr: true, candidate: "1.0.0"},
			{name: "bad listing", listing: "not JSON", candidate: "1.0.0"},
			{name: "missing listing count", listing: `{}`, candidate: "1.0.0"},
			{name: "inconsistent listing", listing: `{"KeyCount":1}`, candidate: "1.0.0"},
			{name: "missing listed key", listing: `{"KeyCount":1,"Contents":[{}]}`, candidate: "1.0.0"},
			{name: "bad manifest", manifest: "not JSON", candidate: "1.0.0"},
			{name: "missing version", manifest: `{}`, candidate: "1.0.0"},
			{name: "empty manifest", candidate: "1.0.0"},
			{name: "invalid version", manifest: `{"version":"garbage"}`, candidate: "1.0.0"},
			{name: "invalid candidate", manifest: `{"version":"1.0.0"}`, candidate: "garbage"},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				calls := 0
				aws := func(_ context.Context, args ...string) ([]byte, error) {
					calls++
					if calls == 1 {
						if args[0] != "s3api" || !strings.Contains(strings.Join(args, " "), "--prefix "+key) {
							t.Fatalf("wrong list call: %v", args)
						}
						if tc.listErr {
							return nil, errors.New("AccessDenied")
						}
						if tc.listing != "" {
							return []byte(tc.listing), nil
						}
						return []byte(`{"KeyCount":1,"Contents":[{"Key":"` + key + `"}]}`), nil
					}
					if calls != 2 || args[0] != "s3" || args[2] != "s3://releases/"+key {
						t.Fatalf("wrong read call: %v", args)
					}
					if tc.readErr {
						return nil, errors.New("network failure")
					}
					return []byte(tc.manifest), nil
				}
				var out bytes.Buffer
				err := run(context.Background(), []string{"--bucket", "releases", "--key", key, "--endpoint", "https://bucket.example", tc.candidate}, &out, aws)
				if (err == nil) != (tc.want != "") || out.String() != tc.want {
					t.Fatalf("output %q, err %v; want %q", out.String(), err, tc.want)
				}
			})
		}
	}
}

func TestPositionalPromotionErrorsPropagate(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"garbage", "1.0.0"}, &out, nil)
	if err == nil || out.Len() != 0 {
		t.Fatalf("invalid current must fail without a decision: %q %v", out.String(), err)
	}
}
