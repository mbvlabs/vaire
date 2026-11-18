set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

alias gfd := go-fix-diff
alias gf := go-fix
alias gcl := golangci-lint

commit := `git rev-parse --short=12 HEAD`
ldflags := "-s -w -X github.com/mbvlabs/vaire/internal/buildinfo.Version=dev -X github.com/mbvlabs/vaire/internal/buildinfo.Commit=" + commit + " -X github.com/mbvlabs/vaire/internal/buildinfo.Channel=dev"

default:
    @just --list

# Build the vaire binary into bin/vaire.
build:
    mkdir -p bin
    CGO_ENABLED=0 go build -trimpath -ldflags "{{ldflags}}" -o bin/vaire ./cmd/vaire

golangci-lint:
    golangci-lint run ./...

go-fix-diff:
	go fix -diff ./...

go-fix:
	go fix ./...

# Lint Go, command metadata, and shell scripts.
lint: go-fix-diff golangci-lint
	@echo "lint done"

# Build an unsigned channel tree under dist/release-<channel>.
release-local VERSION CHANNEL BASE_URL:
    scripts/build-release.sh "{{VERSION}}" "{{CHANNEL}}" "{{BASE_URL}}"
