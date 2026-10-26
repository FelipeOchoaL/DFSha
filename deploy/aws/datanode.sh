#!/usr/bin/env bash
# Redesplega el DataNode de ESTA EC2.
#   cp deploy/aws/env/dn-1.env.example /opt/dfsha/node.env   # y pon la IP pública en ADVERTISE_ADDR
#   deploy/aws/datanode.sh
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
ROOT="$(repo_root)"

load_env
require_vars NODE_ID RACK GRPC_PORT ADVERTISE_ADDR NAMENODE_ADDRS
[[ "$ADVERTISE_ADDR" != *"<IP_PUBLICA>"* ]] || die "cambia <IP_PUBLICA> en ADVERTISE_ADDR por la IP pública de esta EC2"
check_tls

NAME="datanode"
IMAGE="dfsha/datanode:latest"

update_code
docker build -f "$ROOT/services/Dockerfile.datanode" -t "$IMAGE" "$ROOT/services"

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --restart unless-stopped \
  --env-file "$ENV_FILE" \
  -p "${GRPC_PORT}:${GRPC_PORT}" \
  -v /var/lib/dfsha/data:/data \
  ${CERTS_DIR:+-v "${CERTS_DIR}:/certs:ro"} \
  "$IMAGE"

echo "DataNode $NODE_ID rack $RACK arriba. Anuncia $ADVERTISE_ADDR"
echo "Logs: docker logs -f $NAME"
