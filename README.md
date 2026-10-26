# DFSha

Sistema de archivos distribuido por bloques (tipo HDFS) con alta disponibilidad, replicación y seguridad.

## Estructura

```text
proto/          Contratos gRPC compartidos (datanode.proto, namenode.proto)
services/       Go: NameNode y DataNode
  cmd/          Binarios (namenode, datanode)
  internal/     Lógica de cada nodo y utilidades comunes
  gen/          Código generado desde proto/
client/         Python: CLI y SDK (REST al NameNode, gRPC a DataNodes)
scripts/        Generación de código desde proto/
deploy/         Despliegue en AWS y certificados de desarrollo
documentation/  Enunciado, backlog y diagramas PlantUML
```

## Requisitos

| Herramienta | Versión | Para qué |
|---|---|---|
| Go | 1.22+ | NameNode y DataNode |
| Python | 3.11+ | Cliente CLI y generación de código |
| Docker Desktop | reciente | Clúster local |
| `protoc-gen-go` / `protoc-gen-go-grpc` | 1.36 / 1.5 | Stubs Go (ver abajo) |

`protoc` no hace falta instalarlo aparte: lo trae `grpcio-tools`.

No trabajes el repo dentro de OneDrive ni en rutas con espacios: Docker y los scripts fallan. Clónalo por ejemplo en `C:\dev\DFSha`.

```powershell
# Python
python -m venv .venv
.\.venv\Scripts\Activate.ps1
pip install -r client/requirements.txt

# Plugins Go para protoc (quedan en %USERPROFILE%\go\bin)
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
```

## Generar código desde `proto/`

```powershell
.\scripts\gen_proto.ps1      # Windows
./scripts/gen_proto.sh       # Linux / macOS / AWS
```

Genera `services/gen/**` (Go) y `client/gen/**` (Python). **El código generado se commitea.** Si cambias un `.proto`, corre el script y sube lo generado en el mismo PR (etiqueta `contract-change`).

## Cliente

```powershell
$env:DFSHA_NAMENODES = "http://localhost:8080,http://localhost:8081"   # activo,standby
python -m client.main login ana
python -m client.main mkdir -p /user/ana/docs
python -m client.main cd /user/ana/docs
python -m client.main put .\archivo.bin archivo.bin
python -m client.main ls
python -m client.main get archivo.bin .\descarga.bin
python -m client.main rm archivo.bin
```

Variables: `DFSHA_NAMENODES` (lista separada por comas), `DFSHA_BLOCK_SIZE_MB` (8), `DFSHA_HTTP_TIMEOUT` (10), `DFSHA_GRPC_TIMEOUT` (60), `DFSHA_MAX_THREADS` (4), `DFSHA_CA_CERT` (ruta a la CA para TLS), `DFSHA_SESSION_FILE` (`~/.dfsha/session.json`).

El cliente habla REST con el NameNode (metadatos) y gRPC directo con los DataNodes (bloques). Si el NameNode activo no responde o contesta `503`, prueba el siguiente de la lista.

### Pruebas del cliente

```powershell
python -m pytest client/tests -q
```

Incluyen un NameNode y DataNodes falsos (`client/tests/fakes.py`) que respetan los contratos, así que `put`/`get` se prueban de punta a punta sin Go ni Docker.

## Servicios (Go)

```powershell
cd services
go build ./...
go test ./...
```

### NameNode
Un par activo/standby se levanta con los mismos binarios y distinto `NAMENODE_ROLE`. La API REST está en `documentation/api-rest.md`.

| Variable | Por defecto | Uso |
|---|---|---|
| `NAMENODE_ROLE` | `active` | `active` o `standby` (rol inicial; tras un failover el rol efectivo puede cambiar) |
| `REST_PORT` / `GRPC_PORT` | `8080` / `50050` | Puertos de la API REST y del gRPC |
| `ADVERTISE_ADDR` | | Dirección `host:puerto` gRPC propia |
| `PEER_NAMENODE_ADDR` | | gRPC del otro NameNode. Si falta se deduce de `NAMENODE_ADDRS` (la única distinta de `ADVERTISE_ADDR`) |
| `DATA_DIR` / `META_DIR` | `./data` / `DATA_DIR/meta` | Edit log y snapshot del namespace |
| `HA_REQUIRE_ACK` | `true` | Replicación estricta: la escritura falla si el standby no confirma |
| `TLS_ENABLED` | `true` | HTTPS en REST y mTLS en gRPC. Certificados en `/certs/node.crt`, `/certs/node.key`, `/certs/ca.crt` |

Los Dockerfiles de `services/` construyen cada binario estático; corren como root porque el bind mount de `/data` pertenece a root.