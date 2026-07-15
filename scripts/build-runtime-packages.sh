#!/usr/bin/env bash

# Build credential-free Linux runtime archives for transfer to a cron host.

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/.." && pwd)
dist_dir=${AXM2SNIPE_DIST_DIR:-$repo_dir/dist}
version=${AXM2SNIPE_VERSION:-$(git -C "$repo_dir" describe --tags --always --dirty 2>/dev/null || printf 'dev')}

architectures=(amd64 arm64)
if (( $# > 0 )); then
    architectures=("$@")
fi

mkdir -p -- "$dist_dir"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

for arch in "${architectures[@]}"; do
    case "$arch" in
        amd64|arm64) ;;
        *) printf 'Unsupported Linux architecture: %s\n' "$arch" >&2; exit 2 ;;
    esac

    package_root=$work_dir/axm2snipe
    rm -rf -- "$package_root"
    mkdir -p -- "$package_root/scripts" "$package_root/deploy"

    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build \
        -trimpath -ldflags="-s -w -X main.version=$version" \
        -o "$package_root/axm2snipe" "$repo_dir"

    install -m 0755 "$repo_dir/scripts/cached-sync.sh" "$package_root/scripts/cached-sync.sh"
    install -m 0644 "$repo_dir/deploy/axm2snipe.crontab.example" "$package_root/deploy/axm2snipe.crontab.example"
    install -m 0644 "$repo_dir/settings.example.yaml" "$package_root/settings.example.yaml"
    install -m 0644 "$repo_dir/README.md" "$package_root/README.md"
    if [[ -f "$repo_dir/LICENSE.md" ]]; then
        install -m 0644 "$repo_dir/LICENSE.md" "$package_root/LICENSE.md"
    fi

    archive=$dist_dir/axm2snipe-linux-$arch.tar.gz
    tar -C "$work_dir" -czf "$archive" axm2snipe
    sha256sum "$archive" >"$archive.sha256"
    printf 'Built %s\n' "$archive"
done
