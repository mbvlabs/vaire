package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/release"
)

type awsRunner func(context.Context, ...string) ([]byte, error)

// bucketVersion distinguishes an absent pointer from a failed read. Listing
// first avoids treating every AWS CLI error (including AccessDenied and
// network failures) as NoSuchKey. S3 lists keys lexicographically, so an exact
// match, when present, is the first key with this prefix.
func bucketVersion(ctx context.Context, aws awsRunner, bucket, key, endpoint string) (string, error) {
	raw, err := aws(ctx, "s3api", "list-objects-v2", "--bucket", bucket,
		"--prefix", key, "--max-keys", "1", "--no-paginate", "--output", "json", "--endpoint-url", endpoint)
	if err != nil {
		return "", fmt.Errorf("list channel manifest %s: %w", key, err)
	}
	var listing struct {
		KeyCount *int
		Contents []struct{ Key string }
	}
	if err := json.Unmarshal(raw, &listing); err != nil || listing.KeyCount == nil ||
		*listing.KeyCount != len(listing.Contents) || *listing.KeyCount > 1 {
		return "", fmt.Errorf("invalid bucket listing for channel manifest %s", key)
	}
	if len(listing.Contents) == 0 {
		return "", nil
	}
	if !strings.HasPrefix(listing.Contents[0].Key, key) {
		return "", fmt.Errorf("invalid key in bucket listing for channel manifest %s", key)
	}
	if listing.Contents[0].Key != key {
		return "", nil
	}
	raw, err = aws(ctx, "s3", "cp", "s3://"+bucket+"/"+key, "-", "--endpoint-url", endpoint)
	if err != nil {
		return "", fmt.Errorf("read channel manifest %s: %w", key, err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", fmt.Errorf("invalid channel manifest %s: %w", key, err)
	}
	if _, ok := release.ParseStable(manifest.Version); !ok {
		return "", fmt.Errorf("invalid version in channel manifest %s", key)
	}
	return manifest.Version, nil
}
