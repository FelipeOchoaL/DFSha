#!/usr/bin/env bash
# Funciones comunes de namenode.sh y datanode.sh. Se usa con "source", no se ejecuta.
die() { echo "error: $*" >&2; exit 1; }

repo_root() { cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd; }

load_env() {
  ENV_FILE="${ENV_FILE:-$ROOT/deploy/aws/node.env}"
  [[ -f "$ENV_FILE" ]] || die "no existe $ENV_FILE (copia el ejemplo de deploy/aws/env/ y renómbralo a node.env)"
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
}

require_vars() {
  local v missing=()
  for v in "$@"; do [[ -n "${!v:-}" ]] || missing+=("$v"); done
  [[ ${#missing[@]} -eq 0 ]] || die "faltan variables en $ENV_FILE: ${missing[*]}"
}

update_code() {
  if [[ "${NO_PULL:-0}" == "1" ]]; then
    echo "NO_PULL=1: se omite git pull"
  else
    git -C "$ROOT" pull --ff-only
  fi
}

check_tls() {
  if [[ "${TLS_ENABLED:-false}" == "true" ]]; then
    local f
    for f in node.crt node.key ca.crt; do
      [[ -f "${CERTS_DIR:-}/$f" ]] || die "TLS_ENABLED=true pero falta $CERTS_DIR/$f"
    done
  fi
}
