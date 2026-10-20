// Command promote tells scripts/upload-release.sh whether a stable release
// moves a channel manifest: it prints yes or no for CURRENT (the version the
// channel's manifest points at, "" when it has none) and CANDIDATE.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/mbvlabs/vaire/internal/release"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, awsOutput); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func awsOutput(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "aws", args...).Output() //nolint:gosec // G204: fixed executable; bucket/key/endpoint are separate arguments, never shell input.
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
	ok, err := release.Promotes(current, candidate)
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
