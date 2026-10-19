# DataNode

Implementacion del almacenamiento y del servicio definido en `proto/datanode.proto`.

Desde `services/`, con Go 1.22 o posterior:

```powershell
go mod download
go build ./...
go vet ./...
go test -v ./...
go test -count=10 ./...

$env:NODE_ID = "dn-1"
$env:RACK = "rack-a"
$env:GRPC_PORT = "50051"
$env:ADVERTISE_ADDR = "localhost"
$env:DATA_DIR = "./data/dn-1"
go run ./cmd/datanode
```

Ctrl+C solicita el cierre del servidor. Las llamadas en curso disponen de cinco
segundos antes de forzar el cierre.

## Configuracion

| Variable | Valor por defecto |
| --- | --- |
| `NODE_ID` | `datanode-1` |
| `RACK` | `rack-a` |
| `GRPC_PORT` | `50051` |
| `REST_PORT` | `8080` |
| `DATA_DIR` | `./data` |
| `NAMENODE_ADDRS` | `localhost:50050` (lista separada por comas) |
| `ADVERTISE_ADDR` | `localhost` (host anunciado, sin puerto) |
| `BLOCK_SIZE_MB` | `64` |
| `REPLICATION_FACTOR` | `3` |

## Operaciones

- `WriteBlock` recibe un header y chunks, verifica el tamano y SHA-256 y publica
  el bloque mediante archivos temporales y renombrado. Los bloques son
  inmutables: repetir el mismo contenido es valido; cambiarlo con el mismo ID
  devuelve `AlreadyExists`.
- `ReadBlock` verifica el archivo antes de enviar el header y los chunks de
  hasta 64 KiB. `offset` y `length` seleccionan un rango; el header conserva
  el tamano y checksum del bloque completo. `length=0` lee hasta el final.
- `DeleteBlock` es idempotente: para un bloque inexistente devuelve
  `deleted=false` sin error.
- El pipeline escribe en un temporal local y reenvia cada chunk al siguiente
  nodo, conservando el orden. Publica localmente despues de recibir y validar
  todos los ACK. Una falla descarta el temporal y conserva las copias previas.
  Cada salto tiene un limite de 30 segundos. Una falla posterior a un ACK
  puede dejar replicas huerfanas aguas abajo; su limpieza corresponde al
  NameNode cuando se integre el plano de control.

Los bloques se guardan como `<id>.blk` y sus metadatos como `<id>.meta`.
El archivo de metadatos se publica al final para no listar escrituras parciales.

## Pruebas

Las pruebas usan puertos TCP locales asignados por el sistema y directorios
temporales, sin necesitar Docker ni un NameNode. Cubren configuracion, checksum,
persistencia, escrituras invalidas e interrumpidas, concurrencia, rangos de
lectura, corrupcion, borrado y pipeline con tres DataNodes. Tambien verifican
que una falla del ultimo nodo no borre un bloque ya confirmado y que el binario
arranque y cierre mediante el mismo contexto utilizado por Ctrl+C.

Los archivos de `services/gen/datanode/v1/` se generan con protoc 25.3,
`protoc-gen-go` 1.34.1 y `protoc-gen-go-grpc` 1.4.0. Desde la raiz del repo:

```text
protoc -I proto --go_out=services --go_opt=module=dfsha/services --go-grpc_out=services --go-grpc_opt=module=dfsha/services datanode.proto
```

## Alcance

Esta entrega cubre US-06, US-10, US-11, US-12 y el arranque Go de US-13;
adelanta el pipeline de US-51 y DeleteBlock de US-55. El registro, heartbeat,
BlockReceived, comandos del NameNode, failover, tokens, TLS y cifrado en reposo
siguen pendientes de sus contratos e integracion. El transporte actual usa
gRPC sin TLS, destinado al desarrollo local. Las pruebas no validan AWS ni el
DFS completo, cuyo NameNode y cliente aun tienen implementaciones pendientes.
