#!/usr/bin/env bash
set -euo pipefail

usage() {
    echo "Usage: $0 [--type=patch|minor|major]  (default: patch)"
    exit 1
}

type="patch"
for arg in "$@"; do
    case "$arg" in
        --type=patch|--type=minor|--type=major) type="${arg#--type=}" ;;
        *) usage ;;
    esac
done

# Get latest tag (strip leading 'v' if present)
latest=$(git tag --sort=-v:refname | head -1)
version="${latest#v}"

IFS='.' read -r major minor patch <<< "$version"

case "$type" in
    patch) patch=$((patch + 1)) ;;
    minor) minor=$((minor + 1)); patch=0 ;;
    major) major=$((major + 1)); minor=0; patch=0 ;;
esac

new_tag="${major}.${minor}.${patch}"

echo "Current: ${latest}"
echo "New tag: ${new_tag}"
read -rp "Proceed? [y/N] " confirm
[[ "$confirm" != "y" && "$confirm" != "Y" ]] && { echo "Aborted."; exit 0; }

git tag "$new_tag"
git push origin "$new_tag"

echo "Creating GitHub release..."
gh release create "$new_tag" --generate-notes
echo "Released ${new_tag}"
