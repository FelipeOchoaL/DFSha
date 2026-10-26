#!/usr/bin/env bash
# Redesplega el NameNode de ESTA EC2. Las instancias ya existen (ver documentation/guia-aws.md).
#   cp deploy/aws/env/nn-active.env.example /opt/dfsha/node.env   # en dfsha-nn-active
#   deploy/aws/namenode.sh
#   NO_PULL=1 deploy/aws/namenode.sh
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
ROOT="$(repo_root)"

load_env
require_vars NODE_ID NAMENODE_ROLE REST_PORT GRPC_PORT ADVERTISE_ADDR BLOCK_SIZE_MB REPLICATION_FACTOR
[[ "$NAMENODE_ROLE" == "active" || "$NAMENODE_ROLE" == "standby" ]] || die "NAMENODE_ROLE debe ser active o standby"
check_tls

NAME="namenode"
IMAGE="dfsha/namenode:latest"

update_code
docker build -f "$ROOT/services/Dockerfile.namenode" -t "$IMAGE" "$ROOT/services"

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --restart unless-stopped \
  --env-file "$ENV_FILE" \
  -p "${REST_PORT}:${REST_PORT}" -p "${GRPC_PORT}:${GRPC_PORT}" \
  -v /var/lib/dfsha/data:/data \
  ${CERTS_DIR:+-v "${CERTS_DIR}:/certs:ro"} \
  "$IMAGE"

echo "NameNode $NODE_ID ($NAMENODE_ROLE) arriba. REST :$REST_PORT  gRPC $ADVERTISE_ADDR"
echo "Logs: docker logs -f $NAME"
