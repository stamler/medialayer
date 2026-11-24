#!/usr/bin/env bash
# Builds and tests the overlay against another rclone version, to measure
# how much rclone's internal Go API changes between releases.
#
#   scripts/churn.sh v1.75.2
#
# The working tree is not modified: the check runs on a temporary copy.
set -euo pipefail

version="${1:?usage: scripts/churn.sh <rclone version, e.g. v1.75.2>}"
repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cp -R "$repo/." "$work/"
cd "$work"
echo "== rclone $version"
# Start from a fresh go.mod so every dependency comes from that rclone
# release, not from the versions pinned for the current one.
goversion="$(sed -n 's/^go //p' go.mod)"
rm -f go.mod go.sum
go mod init github.com/stamler/medialayer
go mod edit -go="$goversion"
go get "github.com/rclone/rclone@$version"
go mod tidy
if go build ./... && go vet ./... && go test -count=1 ./...; then
	echo "== rclone $version: builds and passes with no changes"
else
	echo "== rclone $version: needs changes (see errors above)"
	exit 1
fi
