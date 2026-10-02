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
documentation/  Enunciado y diagramas PlantUML
```
