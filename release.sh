#!/usr/bin/env bash
# Tags a release. Pushing the tag triggers .github/workflows/release.yml, which
# tests, builds every asset and publishes the GitHub release (plus Homebrew/Scoop).
#
#   ./release.sh [--type=patch|minor|major] [--pre=rc.1]
#
# --pre creates a prerelease tag (e.g. 0.5.0-rc.1): it never becomes "latest",
# and Homebrew/Scoop aren't updated, so it's safe for smoke-testing installers
# (LITELLM_PLUGIN_VERSION=0.5.0-rc.1 bash install.sh).
set -euo pipefail

usage() {
    echo "Usage: $0 [--type=patch|minor|major] [--pre=rc.N]  (default: patch)"
    exit 1
}

type="patch"
pre=""
for arg in "$@"; do
    case "$arg" in
        --type=patch|--type=minor|--type=major) type="${arg#--type=}" ;;
        --pre=*) pre="${arg#--pre=}" ;;
        *) usage ;;
    esac
done

die() { echo "release: $*" >&2; exit 1; }

gh auth status >/dev/null 2>&1 || die "gh is not authenticated (gh auth login)"
[ -z "$(git status --porcelain)" ] || die "working tree is not clean"
[ "$(git rev-parse --abbrev-ref HEAD)" = "main" ] || die "not on main"
git fetch --quiet --tags origin main
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || die "HEAD is not origin/main (push or pull first)"

head_sha=$(git rev-parse HEAD)
ci=$(gh run list --commit "$head_sha" --workflow CI --json conclusion,status --jq '.[0] | (.conclusion // .status)' 2>/dev/null || true)
[ "$ci" = "success" ] || die "CI for ${head_sha:0:7} is '${ci:-missing}', not success"

# Latest final release tag (prereleases don't advance the base version).
latest=$(git tag --sort=-v:refname | grep -E '^v?[0-9]+\.[0-9]+\.[0-9]+$' | head -1)
IFS='.' read -r major minor patch <<< "${latest#v}"
case "$type" in
    patch) patch=$((patch + 1)) ;;
    minor) minor=$((minor + 1)); patch=0 ;;
    major) major=$((major + 1)); minor=0; patch=0 ;;
esac
new_tag="${major}.${minor}.${patch}${pre:+-$pre}"
git rev-parse -q --verify "refs/tags/$new_tag" >/dev/null && die "tag $new_tag already exists"

echo "Current: ${latest}"
echo "New tag: ${new_tag}${pre:+ (prerelease)}"
read -rp "Proceed? [y/N] " confirm
[[ "$confirm" != "y" && "$confirm" != "Y" ]] && { echo "Aborted."; exit 0; }

git tag -a "$new_tag" -m "$new_tag"
git push origin "$new_tag"
echo "Pushed ${new_tag}. Follow the release: gh run watch \$(gh run list --workflow Release --limit 1 --json databaseId --jq '.[0].databaseId')"
