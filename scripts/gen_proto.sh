#!/usr/bin/env bash
# Genera el código desde proto/: stubs Go en services/gen/ y Python en client/gen/ (US-04).
# Versión Linux/macOS/AWS del gen_proto.ps1. Requisitos iguales (grpcio-tools + plugins Go).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

python="python3"
[ -x "$root/.venv/bin/python" ] && python="$root/.venv/bin/python"

export PATH="$HOME/go/bin:$PATH"

protos=(proto/datanode.proto proto/namenode.proto)
mkdir -p services/gen client/gen

echo "==> Go -> services/gen"
"$python" -m grpc_tools.protoc -I proto \
  --go_out=services/gen --go_opt=module=dfsha/services/gen \
  --go-grpc_out=services/gen --go-grpc_opt=module=dfsha/services/gen \
  "${protos[@]}"

echo "==> Python -> client/gen"
"$python" -m grpc_tools.protoc -I proto \
  --python_out=client/gen --pyi_out=client/gen --grpc_python_out=client/gen \
  "${protos[@]}"

echo "OK"
