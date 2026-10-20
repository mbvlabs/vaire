set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

commit := `git rev-parse --short=12 HEAD`
ldflags := "-s -w -X github.com/mbvlabs/vaire/internal/buildinfo.Version=dev -X github.com/mbvlabs/vaire/internal/buildinfo.Commit=" + commit + " -X github.com/mbvlabs/vaire/internal/buildinfo.Channel=dev"

default:
    @just --list

# Build the vaire binary into bin/vaire.
build:
    mkdir -p bin
    CGO_ENABLED=0 go build -trimpath -ldflags "{{ldflags}}" -o bin/vaire ./cmd/vaire

# Lint Go, command metadata, and shell scripts.
lint:
    golangci-lint run ./...
    just build
    bin/vaire commands --check-meta
    bin/vaire commands --check-steps
    bash -eu -o pipefail -c 'shopt -s nullglob; files=(internal/steps/scripts/*.sh scripts/*.sh); if ((${#files[@]})); then shellcheck -x "${files[@]}"; fi'
    actionlint
    bash -eu -o pipefail -c 'while IFS="=" read -r key value; do grep -Fq "$value" scripts/install.sh || { echo "scripts/install.sh is missing $key=$value" >&2; exit 1; }; done < <(go run ./internal/versions/cmd/print)'

# Build an unsigned channel tree under dist/release-<channel>.
release-local VERSION CHANNEL BASE_URL:
    scripts/build-release.sh "{{VERSION}}" "{{CHANNEL}}" "{{BASE_URL}}"
