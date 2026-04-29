#!/usr/bin/env bash
# toggle-remote.sh — switch git origin between GitHub and the local git server

set -euo pipefail

GITHUB="git@github.com:dbkeys/Bitmark-Explorer.git"
LOCAL="git@git:/home/git/Bitmark-Explorer.git"

current=$(git remote get-url origin)

if [[ "$current" == "$GITHUB" ]]; then
    git remote set-url origin "$LOCAL"
    echo "origin → local  ($LOCAL)"
elif [[ "$current" == "$LOCAL" ]]; then
    git remote set-url origin "$GITHUB"
    echo "origin → GitHub ($GITHUB)"
else
    echo "Unrecognised remote URL: $current"
    echo "Expected one of:"
    echo "  $GITHUB"
    echo "  $LOCAL"
    exit 1
fi
