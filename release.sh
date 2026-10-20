#!/bin/bash
# Build release binaries for every supported platform and publish a GitHub
# release.
#
#   ./release.sh [version]      (default: the version in main.go)
#
# Requirements: go, git, gh (authenticated), tar, zip.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
cd "$HERE"

VERSION=${1:-$(sed -n 's/^var version = "\(.*\)"$/\1/p' main.go)}
TAG="v$VERSION"

# The platforms the release ships binaries for.
PLATFORMS="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64"

if [ -n "$(git status --porcelain)" ]; then
  echo "error: the working tree is dirty; commit or stash first" >&2
  exit 1
fi
if [ "$(git rev-parse --abbrev-ref HEAD)" != "main" ]; then
  echo "error: releases are cut from main" >&2
  exit 1
fi
if git rev-parse "origin/$TAG" >/dev/null 2>&1; then
  echo "error: $TAG already exists" >&2
  exit 1
fi

echo "==> releasing $TAG"
rm -rf dist
mkdir -p dist

LDFLAGS="-s -w -X main.version=$VERSION"

for platform in $PLATFORMS; do
  os=${platform%/*}
  arch=${platform#*/}
  name="ggg-${os}-${arch}"
  if [ "$os" = "windows" ]; then
    binary="dist/${name}.exe"
  else
    binary="dist/${name}"
  fi

  echo "==> building $name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$binary" .

  if [ "$os" = "windows" ]; then
    (cd dist && zip -q "${name}_${VERSION}.zip" "$(basename "$binary")")
  else
    (cd dist && tar czf "${name}_${VERSION}.tar.gz" "$(basename "$binary")")
  fi
  rm -f "$binary"
done

echo "==> checksums"
(cd dist && shasum -a 256 ./*.tar.gz ./*.zip > SHA256SUMS)
cat dist/SHA256SUMS

echo "==> verifying the local binary"
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /tmp/ggg-release-check .
/tmp/ggg-release-check -version
rm -f /tmp/ggg-release-check

echo "==> tagging $TAG"
git tag -a "$TAG" -m "ggg $VERSION"
git push origin main
git push origin "$TAG"

echo "==> publishing the release"
gh release create "$TAG" \
  dist/*.tar.gz dist/*.zip dist/SHA256SUMS \
  --title "ggg $VERSION" \
  --notes "$(cat <<NOTES
Find every git repository under a directory and sync each one with its remote.

- fetches as much of the remote as possible
- keeps local changes: uncommitted work, untracked files and local commits
- \`stash push --include-untracked\` → sync → \`stash pop\`
- fast-forwards \`main\`/\`master\`, merges when diverged
- feature branches get their upstream and \`origin/main\` merged in
- conflicts are auto-resolved with \`-X theirs\`; unresolvable merges fall back
  to \`main\` and the feature branch is kept intact
- a remote that is really gone is reported as a local-only repository and
  left completely untouched: nothing is synced, nothing is renamed
- a repository hidden by ggg 1.1 is restored when its remote comes back
- never force-pushes, never \`reset --hard\`, never drops a stash

\`\`\`sh
# download the archive for your platform
tar xzf ggg-darwin-arm64_${VERSION}.tar.gz
./ggg -root ~/src
\`\`\`

Verify a download with:

\`\`\`sh
shasum -a 256 -c SHA256SUMS
\`\`\`
NOTES
)"

echo
echo "==> done: https://github.com/thinpo/ggg/releases/tag/$TAG"
