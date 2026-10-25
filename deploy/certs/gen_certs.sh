#!/usr/bin/env bash
# Certificados TLS de DESARROLLO para DFSha: una CA propia y un certificado por nodo.
#
# Uso:
#   deploy/certs/gen_certs.sh ca                          crea la CA (ca.crt, ca.key)
#   deploy/certs/gen_certs.sh node <nombre> [SAN ...]     certificado de un nodo
#   deploy/certs/gen_certs.sh local                       CA + los 6 nodos del docker-compose
#   deploy/certs/gen_certs.sh from-file <archivo>         CA + los nodos listados en el archivo
#   deploy/certs/gen_certs.sh verify                      comprueba que cada certificado valida contra la CA
#
# SAN es "DNS:nombre" o "IP:1.2.3.4". Siempre se incluyen DNS:<nombre>, DNS:localhost,
# DNS:host.docker.internal e IP:127.0.0.1 (desarrollo local con Docker).
#
# Formato de <archivo> (una línea por nodo; # comenta):
#   namenode-1   DNS:ec2-3-80-1-2.compute-1.amazonaws.com  IP:10.0.1.10
#   datanode-1   DNS:ec2-3-80-1-3.compute-1.amazonaws.com  IP:10.0.1.11
#
# Resultado (en OUT_DIR, por defecto esta misma carpeta):
#   ca.crt                      certificado público de la CA (lo necesitan TODOS, incluido el cliente)
#   ca.key, ca.srl              clave privada de la CA y su contador: NO salen de tu máquina
#   <nodo>/node.crt             certificado del nodo
#   <nodo>/node.key             clave privada del nodo
#   <nodo>/ca.crt               copia de la CA para montar junto al certificado en /certs
#
# Variables: OUT_DIR, DAYS_CA (1095), DAYS_NODE (365), KEY_MODE (600; 644 en "local").
# Las claves privadas están en .gitignore: solo se sube este script.
# Requiere: bash y openssl >= 1.1.1 (en Windows, Git Bash o WSL).

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${OUT_DIR:-$HERE}"
DAYS_CA="${DAYS_CA:-1095}"
DAYS_NODE="${DAYS_NODE:-365}"
KEY_MODE="${KEY_MODE:-600}"
LOCAL_NODES=(nn1 nn2 dn1 dn2 dn3 dn4)

die() { echo "error: $*" >&2; exit 1; }
need_openssl() { command -v openssl >/dev/null 2>&1 || die "no se encontró openssl en el PATH"; }

usage() { sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'; }

cmd_ca() {
  need_openssl
  mkdir -p "$OUT_DIR"
  if [[ -f "$OUT_DIR/ca.crt" && -f "$OUT_DIR/ca.key" ]]; then
    echo "CA ya existe en $OUT_DIR (no se regenera; bórrala si quieres empezar de cero)"
    return
  fi
  openssl ecparam -name prime256v1 -genkey -noout -out "$OUT_DIR/ca.key"
  local ext; ext="$(mktemp)"
  printf 'basicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\nsubjectKeyIdentifier=hash\n' >"$ext"
  openssl req -new -key "$OUT_DIR/ca.key" -subj "/O=DFSha/CN=DFSha Dev CA" -out "$OUT_DIR/ca.csr"
  openssl x509 -req -in "$OUT_DIR/ca.csr" -signkey "$OUT_DIR/ca.key" -sha256 -days "$DAYS_CA" \
    -extfile "$ext" -out "$OUT_DIR/ca.crt" 2>/dev/null
  rm -f "$ext" "$OUT_DIR/ca.csr"
  chmod 600 "$OUT_DIR/ca.key"
  echo "CA creada: $OUT_DIR/ca.crt"
}

cmd_node() {
  [[ $# -ge 1 ]] || die "falta el nombre del nodo"
  need_openssl
  local name="$1"; shift
  [[ "$name" =~ ^[A-Za-z0-9._-]+$ ]] || die "nombre de nodo inválido: '$name' (solo letras, números, . _ -)"
  [[ -f "$OUT_DIR/ca.crt" && -f "$OUT_DIR/ca.key" ]] || die "no hay CA en $OUT_DIR; ejecuta primero: $0 ca"

  local sans="DNS:$name,DNS:localhost,DNS:host.docker.internal,IP:127.0.0.1" s
  for s in "$@"; do
    [[ "$s" =~ ^(DNS:[A-Za-z0-9.*-]+|IP:[0-9a-fA-F.:]+)$ ]] || die "SAN inválido: '$s' (usa DNS:nombre o IP:a.b.c.d)"
    sans+=",$s"
  done

  local dir="$OUT_DIR/$name" ext
  mkdir -p "$dir"
  ext="$(mktemp)"
  {
    echo "subjectAltName=$sans"
    echo "basicConstraints=critical,CA:FALSE"
    echo "keyUsage=critical,digitalSignature"
    echo "extendedKeyUsage=serverAuth,clientAuth" # servidor y cliente: sirve para mTLS entre nodos
  } >"$ext"

  openssl ecparam -name prime256v1 -genkey -noout -out "$dir/node.key"
  openssl req -new -key "$dir/node.key" -subj "/O=DFSha/CN=$name" -out "$dir/node.csr"
  openssl x509 -req -in "$dir/node.csr" -CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
    -sha256 -days "$DAYS_NODE" -extfile "$ext" -out "$dir/node.crt" 2>/dev/null
  rm -f "$ext" "$dir/node.csr"
  cp "$OUT_DIR/ca.crt" "$dir/ca.crt"
  chmod "$KEY_MODE" "$dir/node.key"
  chmod 644 "$dir/node.crt" "$dir/ca.crt"
  echo "nodo $name -> $dir  (SAN: $sans)"
}

cmd_local() {
  # Las claves quedan legibles (644) para que el usuario no-root de los contenedores
  # pueda leer el bind mount en Linux. Es SOLO para desarrollo local.
  KEY_MODE="${KEY_MODE_LOCAL:-644}"
  echo "aviso: claves con permisos $KEY_MODE (solo desarrollo local)" >&2
  cmd_ca
  local n
  for n in "${LOCAL_NODES[@]}"; do cmd_node "$n"; done
}

cmd_from_file() {
  [[ $# -eq 1 && -f "$1" ]] || die "uso: $0 from-file <archivo>"
  cmd_ca
  local line name rest
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%%#*}"; line="${line%$'\r'}"
    [[ -z "${line//[[:space:]]/}" ]] && continue
    read -r name rest <<<"$line"
    # shellcheck disable=SC2086  # los SAN van separados por espacios a propósito
    cmd_node "$name" $rest
  done <"$1"
}

cmd_verify() {
  need_openssl
  local crt n=0
  for crt in "$OUT_DIR"/*/node.crt; do
    [[ -f "$crt" ]] || continue
    openssl verify -CAfile "$OUT_DIR/ca.crt" -purpose sslserver "$crt" >/dev/null || die "no valida: $crt"
    openssl verify -CAfile "$OUT_DIR/ca.crt" -purpose sslclient "$crt" >/dev/null || die "no valida como cliente: $crt"
    echo "ok  $crt"; n=$((n + 1))
  done
  [[ $n -gt 0 ]] || die "no hay certificados de nodo en $OUT_DIR"
}

case "${1:-}" in
  ca)        shift; cmd_ca "$@" ;;
  node)      shift; cmd_node "$@" ;;
  local)     shift; cmd_local "$@" ;;
  from-file) shift; cmd_from_file "$@" ;;
  verify)    shift; cmd_verify "$@" ;;
  -h|--help|help|"") usage ;;
  *) usage >&2; exit 2 ;;
esac
