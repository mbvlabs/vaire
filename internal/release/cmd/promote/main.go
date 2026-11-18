// Command promote tells scripts/upload-release.sh whether a release
// moves a channel manifest: it prints yes or no for CURRENT (the version the
// channel's manifest points at, "" when it has none) and CANDIDATE.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/mbvlabs/vaire/internal/release"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, awsOutput); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func awsOutput(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "aws", args...).Output()
}

// With bucket flags, discover the current pointer fail-closed. The positional
// CURRENT CANDIDATE form remains useful for checking promotion policy locally.
func run(ctx context.Context, args []string, out io.Writer, aws awsRunner) error {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bucket := fs.String("bucket", "", "release bucket")
	key := fs.String("key", "manifest.json", "channel manifest key")

	endpoint := fs.String("endpoint", "", "S3 endpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var current, candidate string

	switch {
	case *bucket != "" && *endpoint != "" && fs.NArg() == 1:
		var err error

		current, err = bucketVersion(ctx, aws, *bucket, *key, *endpoint)
		if err != nil {
			return err
		}

		candidate = fs.Arg(0)
	case *bucket == "" && *endpoint == "" && fs.NArg() == 2:
		current, candidate = fs.Arg(0), fs.Arg(1)
	default:
		return fmt.Errorf("usage: promote CURRENT CANDIDATE, or promote --bucket BUCKET --key KEY --endpoint URL CANDIDATE")
	}

	ok, err := shouldPromote(ctx, *key, current, candidate, gitAncestor, edgeCommit)
	if err != nil {
		return err
	}

	if ok {
		_, err = fmt.Fprintln(out, "yes")
	} else {
		_, err = fmt.Fprintln(out, "no")
	}

	return err
}

func shouldPromote(ctx context.Context, key, current, candidate string, ancestor func(context.Context, string, string) (bool, error), resolve func(context.Context, string) (string, error)) (bool, error) {
	channel := release.ChannelStable

	switch key {
	case "manifest.json":
	case "edge/manifest.json":
		channel = release.ChannelEdge
	case "rc/manifest.json":
		channel = release.ChannelRC
	default:
		line, ok := strings.CutSuffix(key, "/manifest.json")
		if !ok {
			return false, fmt.Errorf("invalid channel manifest key %q", key)
		}

		channel = "stable-" + line
	}

	if _, err := release.Identity(channel, candidate); err != nil {
		return false, err
	}

	if current == "" {
		return true, nil
	}

	if _, err := release.Identity(channel, current); err != nil {
		return false, err
	}

	if channel != release.ChannelEdge {
		return release.CompareVersions(candidate, current) > 0, nil
	}

	return edgePromotes(ctx, current, candidate, ancestor, resolve)
}

func edgePromotes(ctx context.Context, current, candidate string, ancestor func(context.Context, string, string) (bool, error), resolve func(context.Context, string) (string, error)) (bool, error) {
	if candidate == current {
		return false, nil
	}

	from, err := resolve(ctx, current)
	if err != nil {
		return false, err
	}

	to, err := resolve(ctx, candidate)
	if err != nil {
		return false, err
	}

	if from == to {
		if strings.HasPrefix(candidate, "edge-") {
			return false, nil
		}

		return strings.HasPrefix(current, "edge-") || release.CompareVersions(candidate, current) > 0, nil
	}

	return ancestor(ctx, from, to)
}

func edgeCommit(ctx context.Context, version string) (string, error) {
	if _, err := release.Identity(release.ChannelEdge, version); err != nil {
		return "", err
	}

	ref := "refs/tags/v" + version
	if hash, ok := strings.CutPrefix(version, "edge-"); ok {
		ref = hash
	}

	raw, err := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--end-of-options", ref+"^{commit}").Output() //nolint:gosec // The channel validator restricts refs to a hash or exact release tag.
	if err != nil {
		return "", fmt.Errorf("resolve edge source for %q: %w", version, err)
	}

	return strings.TrimSpace(string(raw)), nil
}

func gitAncestor(ctx context.Context, from, to string) (bool, error) {
	err := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", from, to).Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
		return false, nil
	}

	return err == nil, err
}
