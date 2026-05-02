#!/usr/bin/env bash
set -euo pipefail

exec go run ./cmd/firedoze-image build "$@"
