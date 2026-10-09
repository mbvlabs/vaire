set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

commit := `git rev-parse --short=12 HEAD`
ldflags := "-s -w -X github.com/mbvlabs/eru/internal/buildinfo.Version=dev -X github.com/mbvlabs/eru/internal/buildinfo.Commit=" + commit + " -X github.com/mbvlabs/eru/internal/buildinfo.Channel=dev"

default:
    @just --list

# Build the eru binary into bin/eru.
build:
    mkdir -p bin
    CGO_ENABLED=0 go build -trimpath -ldflags "{{ldflags}}" -o bin/eru ./cmd/eru

# Lint Go, command metadata, and shell scripts.
lint:
    golangci-lint run ./...
    just build
    bin/eru commands --check-meta
    bash -eu -o pipefail -c 'shopt -s nullglob; files=(internal/steps/scripts/*.sh scripts/*.sh); if ((${#files[@]})); then shellcheck -x "${files[@]}"; fi'

# Run the end-to-end suite. Requires incus and docker.
e2e: build
    ERU_E2E_BINARY={{justfile_directory()}}/bin/eru go test -tags e2e -count=1 -timeout 90m ./e2e/...
