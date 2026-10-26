# DFSha

Sistema de archivos distribuido por bloques (tipo HDFS): un NameNode guarda metadatos, varios DataNodes guardan los bytes, y el cliente Python nunca mueve el archivo por el maestro.

- **Partición:** el archivo se corta en bloques de 8 MB. Bloques distintos van a DataNodes distintos.
- **Replicación:** cada bloque se copia en **3** DataNodes, en al menos **2 racks**, por un pipeline (cliente → DN1 → DN2 → DN3).
- **Consistencia:** el NameNode solo acepta el bloque cuando las 3 réplicas confirmaron el mismo SHA-256.

---

## Cómo funciona

El usuario ve rutas tipo `/demo/archivo.bin`. No ve IPs ni números de bloque.

```text
CLI Python
   │  REST  (dónde guardar / dónde leer)
   ▼
NameNode activo  ←── gRPC edit log ──→  NameNode standby
   │  gRPC (heartbeat, block report)
   ▼
DataNode 1 (rack A) ──pipeline──→ DataNode 2 (rack A) ──→ DataNode 3 (rack B)
```

El NameNode **no transporta bytes**. El cliente pregunta metadatos por REST y habla gRPC directo con los DataNodes.

### Partición

1. El CLI parte el archivo local en bloques de `DFSHA_BLOCK_SIZE_MB` (8 MB). Un archivo de 20 MB son 3 bloques.
2. Por cada bloque pide `POST /files/allocate`. El NameNode elige 3 DataNodes **vivos y distintos**:
   - el primero (primary) no es el mismo que el del bloque anterior del mismo archivo;
   - las otras dos réplicas priorizan **otro rack**.
3. El archivo queda repartido: ningún DataNode tiene todos los bloques.

### Replicación

1. El cliente envía el bloque solo a DN1 (`WriteBlock` + SHA-256).
2. DN1 lo escribe, lo reenvía a DN2, DN2 a DN3. El ACK sube por la cadena.
3. Cada DataNode avisa al NameNode (`BlockReceived`).
4. `POST /files/commit` espera hasta 3 s a **3 réplicas con el mismo checksum**. Si no, 409 y el CLI pide otro pipeline.
5. Si un DataNode muere (3 heartbeats perdidos), el NameNode marca sus bloques *under-replicated* y le ordena a un nodo que sí los tiene copiarlos a otro (`ReplicateCommand` en el heartbeat).

### Lectura

`GET /files/locations` devuelve, por bloque, las réplicas vivas. El CLI baja los bloques en paralelo. Si una réplica falla o el checksum no coincide, prueba la siguiente. El NameNode no interviene en la transferencia.

---

## Requisitos

| Herramienta | Versión | Uso |
|---|---|---|
| Go | 1.22+ | NameNode y DataNode |
| Python | 3.11+ | CLI y generación de stubs |
| Docker Desktop | reciente | Clúster local |
| `protoc-gen-go` / `protoc-gen-go-grpc` | 1.36 / 1.5 | Stubs Go |

`protoc` no se instala aparte: lo trae `grpcio-tools`.

No trabajes el repo en OneDrive ni en rutas con espacios. Clónalo por ejemplo en `C:\dev\DFSha`.

```powershell
python -m venv .venv
.\.venv\Scripts\Activate.ps1
pip install -r client/requirements.txt

go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
```

---

## Levantar el clúster local

```powershell
docker compose up --build
```

Quedan 2 NameNodes (activo `:8080`, standby `:8081`) y 4 DataNodes en racks A/A/B/B (puertos `50051`–`50054`). TLS va apagado.

```powershell
$env:DFSHA_NAMENODES = "http://localhost:8080,http://localhost:8081"
python -m client.main mkdir /demo
python -m client.main put .\archivo.bin /demo/archivo.bin
python -m client.main ls /demo
python -m client.main get /demo/archivo.bin .\copia.bin
```

Los DataNodes se anuncian como `host.docker.internal:<puerto>` para que el CLI en tu máquina y el pipeline entre contenedores usen los puertos publicados.

Otros comandos: `login`, `cd`, `pwd`, `rmdir`, `rm`.

| Variable del cliente | Por defecto | Qué es |
|---|---|---|
| `DFSHA_NAMENODES` | `http://localhost:8080` | URLs REST, activo y standby separados por coma |
| `DFSHA_BLOCK_SIZE_MB` | `8` | Tamaño de bloque |
| `DFSHA_HTTP_TIMEOUT` | `10` | Segundos hacia el NameNode |
| `DFSHA_GRPC_TIMEOUT` | `60` | Segundos hacia un DataNode |
| `DFSHA_MAX_THREADS` | `4` | Descargas en paralelo |
| `DFSHA_CA_CERT` | | CA para TLS (Hito 3) |
| `DFSHA_SESSION_FILE` | `~/.dfsha/session.json` | Token y directorio actual |

Si el NameNode activo no responde o contesta `503`, el CLI prueba el siguiente de la lista.

---

## Compilar y probar sin Docker

```powershell
# Stubs (solo si cambiaste un .proto; lo generado se commitea)
.\scripts\gen_proto.ps1          # Windows
./scripts/gen_proto.sh           # Linux / AWS

cd services
go build ./...
go test ./...

python -m pytest client/tests -q
```

Las pruebas del cliente usan NameNode y DataNodes falsos (`client/tests/fakes.py`) y cubren `put`/`get` sin Go.

---

## NameNode (variables)

Un par activo/standby usa el mismo binario y distinto `NAMENODE_ROLE`. Rutas REST: `documentation/api-rest.md`.

| Variable | Por defecto | Uso |
|---|---|---|
| `NAMENODE_ROLE` | `active` | `active` o `standby` |
| `REST_PORT` / `GRPC_PORT` | `8080` / `9000` | REST para el cliente, gRPC para DataNodes y HA |
| `ADVERTISE_ADDR` | | `host:puerto` gRPC de este NameNode |
| `PEER_NAMENODE_ADDR` | | El otro NameNode; si falta, se deduce de `NAMENODE_ADDRS` |
| `REPLICATION_FACTOR` | `3` | Réplicas por bloque |
| `DATA_DIR` / `META_DIR` | `./data` / `DATA_DIR/meta` | Edit log y snapshot |
| `HA_REQUIRE_ACK` | `true` | En compose local va `false` |
| `TLS_ENABLED` | `true` | En compose y en el primer AWS va `false` |

---

## AWS

Las 6 EC2 (`dfsha-nn-active` `172.31.0.10`, standby `.11`, `dn-1`…`dn-4` `.21`–`.24`) y el security group están en `documentation/guia-aws.md`.

En cada máquina, después de `git pull` en `main`:

```bash
cp deploy/aws/env/nn-active.env.example /opt/dfsha/node.env   # o dn-1.env.example, etc.
# En DataNodes: cambia <IP_PUBLICA> por la IP pública de ESA instancia
ENV_FILE=/opt/dfsha/node.env deploy/aws/namenode.sh           # o datanode.sh
```

`ADVERTISE_ADDR` del DataNode **tiene que ser la IP pública**: es la que el NameNode le entrega al cliente que corre en tu PC.

---

## Estructura del repo

```text
proto/            Contratos gRPC (datanode.proto, namenode.proto)
services/         Go: NameNode, DataNode, Dockerfiles
  cmd/            Binarios
  internal/       Lógica (namespace, colocación, pipeline, heartbeat)
  gen/            Código generado; no editar a mano
client/           CLI Python (REST al NameNode, gRPC a DataNodes)
scripts/          gen_proto.ps1 / gen_proto.sh
deploy/aws/       Scripts y .env de cada EC2
deploy/certs/     gen_certs.sh (Hito 3)
documentation/    Enunciado, backlog, guía AWS, diagramas
docker-compose.yml
```
