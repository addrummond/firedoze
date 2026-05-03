#!/bin/sh
# Ad-hoc development helper only. This is for quickly syncing a local checkout
# to a test Linux server; it is not part of firedoze installation or release.
set -eu

usage() {
  echo "usage: ./sync-dev.sh [user@]host:/path/to/firedoze/" >&2
  echo "       SSH_KEY=/path/to/key ./sync-dev.sh [user@]host:/path/to/firedoze/" >&2
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  usage
  exit 0
fi

if [ "$#" -ne 1 ]; then
  usage
  exit 1
fi

dest=$1

if [ ! -f go.mod ] || [ ! -d cmd ] || [ ! -d internal ]; then
  echo "error: run this script from the firedoze repository root" >&2
  exit 1
fi

ssh_args=ssh
if [ "${SSH_KEY:-}" != "" ]; then
  ssh_args="ssh -i $SSH_KEY"
fi

rsync -az --delete \
  -e "$ssh_args" \
  --exclude /.git \
  --exclude /dist \
  --exclude /firedoze \
  --exclude /firedozed \
  --exclude /firedoze-image-builder \
  ./ "$dest"
