#!/usr/bin/env bash
# run.sh — Démarre context-vault si le port n'est pas déjà actif.
# Appelé par SessionStart. Retourne dès que le serveur est prêt.
set -euo pipefail

PORT=9742
TIMEOUT=5

# Si le serveur tourne déjà, rien à faire
if curl -sf --max-time 1 "http://localhost:${PORT}/health" >/dev/null 2>&1; then
    exit 0
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "${SCRIPT_DIR}")"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

# Normalise l'architecture
case "${ARCH}" in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
    arm64)   ARCH="arm64" ;;
esac

BIN="${REPO_DIR}/bin/context-vault-${OS}-${ARCH}"

if [ ! -x "${BIN}" ]; then
    echo "[vault] binaire introuvable : ${BIN}" >&2
    echo "[vault] build : go build -o ${BIN} ./cmd/context-vault" >&2
    exit 1
fi

# Résout le project dir — toujours le parent du dossier context-vault,
# indépendamment de CLAUDE_PROJECT_DIR (qui peut contenir un ancien chemin).
PROJ_DIR="$(dirname "${REPO_DIR}")"

# Démarre en background, redirige stderr vers log
LOG="${PROJ_DIR}/.claude/vault.log"
CLAUDE_PROJECT_DIR="${PROJ_DIR}" "${BIN}" >> "${LOG}" 2>&1 &

# Attend la ligne [vault] ready
elapsed=0
while [ "${elapsed}" -lt "${TIMEOUT}" ]; do
    if curl -sf --max-time 1 "http://localhost:${PORT}/health" >/dev/null 2>&1; then
        exit 0
    fi
    sleep 0.2
    elapsed=$((elapsed + 1))
done

echo "[vault] timeout: le serveur n'a pas démarré en ${TIMEOUT}s" >&2
exit 1
