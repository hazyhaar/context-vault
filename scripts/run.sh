#!/usr/bin/env bash
# run.sh — Lance context-vault (Python)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MAIN="${SCRIPT_DIR}/../cmd/context-vault/main.py"

exec python3 "${MAIN}" "$@"
