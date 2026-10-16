// Command promote tells scripts/upload-release.sh whether a stable release
// moves a channel manifest: it prints yes or no for CURRENT (the version the
// channel's manifest points at, "" when it has none) and CANDIDATE.
package main

import (
	"fmt"
	"os"

	"github.com/mbvlabs/eru/internal/release"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: promote CURRENT CANDIDATE")
		os.Exit(2)
	}
	ok, err := release.Promotes(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if ok {
		fmt.Println("yes")
		return
	}
	fmt.Println("no")
}
