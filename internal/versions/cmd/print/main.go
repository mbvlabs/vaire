package main

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/versions"
)

func main() {
	fmt.Printf("cosign_version=%s\n", versions.CosignVersion)
	for _, target := range versions.CosignTargets {
		fmt.Printf("cosign_sha256_%s=%s\n", strings.ReplaceAll(target, "-", "_"), versions.CosignSHA256[target])
	}
}
