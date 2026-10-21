# DFSha — Especificación de la API REST del NameNode

Contrato entre el cliente (CLI/SDK Python) y el NameNode. El cliente se programa contra este documento sin esperar a que el servidor esté listo.

- Responsable del contrato: James (NameNode). Consumidor principal: Felipe (cliente).
- Esta API **solo transporta metadatos**. Los bytes de los archivos viajan por gRPC entre el cliente y los DataNodes (ver `proto/datanode.proto`).
- Este documento es la base de la sección "Especificación de APIs" del informe técnico.

---

## 1. Convenciones generales

| Aspecto | Regla |
|---|---|
| URL base | `http(s)://<namenode>:<REST_PORT>` (por defecto `8080`). HTTPS cuando `TLS_ENABLED=true`. |
| Formato | JSON UTF-8. Las peticiones con body llevan `Content-Type: application/json`. |
| Nombres de campos | `camelCase`. |
| Autenticación | `Authorization: Bearer <jwt>` en **todas** las rutas excepto `POST /auth/login`. |
| Rutas del DFS | Absolutas, empiezan por `/`, separador `/`. El servidor las normaliza (`/a//b/../c` → `/a/c`). Máximo 4096 caracteres. |
| Fechas | RFC 3339 en UTC (`2026-03-01T15:04:05Z`). |
| Tamaños | Enteros en bytes. |
| Checksums | SHA-256 en hexadecimal minúscula (64 caracteres). |
| Parámetros de ruta en GET/DELETE | Van en query string (`?path=/user/ana`) y deben ir codificados como URL. |

### 1.1 Formato de error común

Toda respuesta con código HTTP 4xx o 5xx tiene este cuerpo:

```json
{
  "error": "el directorio no existe: /user/ana/docs",
  "code": "NOT_FOUND"
}
```

- `error`: mensaje legible para mostrar al usuario.
- `code`: identificador estable en `MAYÚSCULAS_CON_GUION_BAJO` para que el cliente decida qué hacer sin interpretar el texto.

### 1.2 Catálogo de códigos de error

| HTTP | `code` | Cuándo ocurre |
|---|---|---|
| 400 | `INVALID_REQUEST` | JSON mal formado, campo obligatorio ausente o con valor inválido. |
| 400 | `INVALID_PATH` | Ruta vacía, relativa, demasiado larga, con caracteres no permitidos o que apunta a la raíz en una operación destructiva. |
| 400 | `IS_A_DIRECTORY` | Se usó `rm` sobre un directorio (usar `rmdir`). |
| 401 | `UNAUTHORIZED` | Falta el token, está mal formado, expiró o la firma es inválida. |
| 401 | `INVALID_CREDENTIALS` | Usuario o contraseña incorrectos en el login. |
| 403 | `FORBIDDEN` | El usuario no tiene permiso sobre la ruta (ver sección 4.6), o intenta continuar la subida de otro usuario. |
| 404 | `NOT_FOUND` | La ruta, el archivo o el directorio no existe. |
| 404 | `PARENT_NOT_FOUND` | Un directorio padre no existe y no se pidió crearlo. |
| 409 | `ALREADY_EXISTS` | La ruta ya existe (archivo `COMPLETE` o directorio). |
| 409 | `FILE_BEING_WRITTEN` | La ruta ya tiene una escritura en curso (`UNDER_CONSTRUCTION`) de otra subida. |
| 409 | `FILE_UNDER_CONSTRUCTION` | Se intenta leer un archivo que aún no está `COMPLETE`. |
| 409 | `NOT_A_DIRECTORY` | Se esperaba un directorio y la ruta (o un padre) es un archivo. |
| 409 | `DIRECTORY_NOT_EMPTY` | `rmdir` sobre un directorio con contenido. |
| 409 | `BLOCK_OUT_OF_ORDER` | `allocate` de un `blockIndex` que no es el siguiente esperado. |
| 409 | `BLOCK_NOT_REPLICATED` | `commit` de un bloque que aún no tiene `REPLICATION_FACTOR` réplicas confirmadas con el mismo checksum, tras esperar el tiempo máximo (ver sección 5.5). El cliente debe reintentar el bloque con un nuevo `allocate`. |
| 409 | `INCOMPLETE_FILE` | `finalize` de un archivo con un bloque asignado pero sin confirmar (sin `commit`). |
| 405 | `METHOD_NOT_ALLOWED` | Se usó un método HTTP distinto al del endpoint. La respuesta incluye la cabecera `Allow`. Una ruta desconocida responde `404 NOT_FOUND`. |
| 500 | `INTERNAL_ERROR` | Error inesperado del servidor. |
| 503 | `NAMENODE_STANDBY` | El NameNode contactado es el standby (ver sección 1.4). |
| 503 | `NAMENODE_RECOVERING` | NameNode recién promovido que espera los block reports de los DataNodes. Afecta solo a `allocate` y `locations`; el resto de la API funciona. Es transitorio (segundos): ver sección 1.4. |
| 503 | `INSUFFICIENT_DATANODES` | No hay suficientes DataNodes vivos para cumplir el factor de réplica. |

### 1.3 Códigos de éxito

| HTTP | Significado |
|---|---|
| 200 | Operación correcta con cuerpo de respuesta. |
| 201 | Recurso creado (mkdir, finalize). |
| 204 | Operación correcta sin cuerpo (rmdir, rm). |

### 1.4 NameNode standby y failover

Un NameNode en rol **standby** responde a **todas** las rutas REST (incluido `POST /auth/login`) con:

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 1
Content-Type: application/json

{"error": "este NameNode es standby", "code": "NAMENODE_STANDBY"}
```

Regla para el cliente (US-73): ante timeout, error de conexión o un `503` con `code = NAMENODE_STANDBY`, probar la siguiente URL de `DFSHA_NAMENODES`. Un `503` con `code = INSUFFICIENT_DATANODES` **no** es señal de failover: el NameNode es el activo, simplemente faltan DataNodes; el cliente debe mostrar el error o reintentar más tarde sin cambiar de NameNode.

Tras un failover, el NameNode promovido no conoce todavía en qué DataNodes están las réplicas (esa información no se persiste). Pide a los DataNodes que se registren de nuevo y, hasta recibir sus block reports (como máximo `RECOVERY_WAIT_SEC`, 30 s por defecto), responde `503 NAMENODE_RECOVERING` con `Retry-After: 1` a `allocate` y `locations`. Igual que `INSUFFICIENT_DATANODES`, **no** es señal para cambiar de NameNode: el cliente debe reintentar contra la misma URL tras la espera indicada. Las demás rutas (`ls`, `mkdir`, `rm`...) responden con normalidad.

---

## 2. Resumen de endpoints

| Método | Ruta | Descripción | Éxito |
|---|---|---|---|
| POST | `/auth/login` | Obtener un JWT | 200 |
| GET | `/fs/ls` | Listar un directorio | 200 |
| POST | `/fs/mkdir` | Crear un directorio | 201 |
| DELETE | `/fs/rmdir` | Eliminar un directorio vacío | 204 |
| DELETE | `/fs/rm` | Eliminar un archivo y sus bloques | 204 |
| POST | `/fs/chmod` | Cambiar los permisos de un archivo o directorio | 200 |
| POST | `/files/allocate` | Pedir dónde escribir un bloque | 200 |
| POST | `/files/commit` | Confirmar un bloque escrito | 200 |
| POST | `/files/finalize` | Cerrar el archivo (`COMPLETE`) | 201 |
| GET | `/files/locations` | Bloques de un archivo y dónde están | 200 |

---

## 3. Autenticación

### 3.1 `POST /auth/login`

No requiere token.

**Request**

```json
{
  "username": "ana",
  "password": "secreto"
}
```

**Response 200**

```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "tokenType": "Bearer",
  "expiresAt": "2026-03-01T16:04:05Z",
  "user": "ana",
  "groups": ["estudiantes"]
}
```

**Errores:** `400 INVALID_REQUEST`, `401 INVALID_CREDENTIALS`, `503 NAMENODE_STANDBY`.

El JWT contiene usuario, grupos y expiración. Una vez expirado, el cliente debe volver a hacer login. En el primer login se crea `/user/<usuario>` como directorio del usuario (US-82).

---

## 4. Sistema de archivos

### 4.1 `GET /fs/ls?path=<ruta>`

Lista el contenido de un directorio. Si `path` es un archivo, devuelve una sola entrada con ese archivo (como `ls archivo` en Linux). Si se omite `path`, se usa `/`.

**Response 200**

```json
{
  "path": "/user/ana",
  "entries": [
    {
      "name": "docs",
      "type": "dir",
      "size": 0,
      "owner": "ana",
      "group": "estudiantes",
      "mode": "0750",
      "permissions": "rwxr-x---",
      "modifiedAt": "2026-03-01T15:04:05Z"
    },
    {
      "name": "datos.csv",
      "type": "file",
      "size": 20971520,
      "owner": "ana",
      "group": "estudiantes",
      "mode": "0640",
      "permissions": "rw-r-----",
      "modifiedAt": "2026-03-01T15:10:00Z",
      "state": "COMPLETE"
    }
  ]
}
```

- `type`: `"dir"` o `"file"`.
- `state`: solo para archivos; `COMPLETE` o `UNDER_CONSTRUCTION`.
- Las entradas vienen ordenadas por nombre.
- En directorios `size` es `0`.
- `group` es el grupo del archivo (se omite si no tiene). `mode` son los permisos en octal y `permissions` los mismos en formato `rwxr-x---` (dueño, grupo, otros).
- Listar un directorio exige permiso de lectura (`r`) sobre él; ver sección 4.6.

**Errores:** `400 INVALID_PATH`, `401`, `403 FORBIDDEN`, `404 NOT_FOUND`.

### 4.2 `POST /fs/mkdir`

**Request**

```json
{
  "path": "/user/ana/docs/2026",
  "parents": true
}
```

- `parents` (opcional, por defecto `false`): si es `true`, crea los directorios padre que falten (como `mkdir -p`) y no falla si el directorio ya existe.

**Response 201**

```json
{
  "path": "/user/ana/docs/2026"
}
```

Con `parents = true` y el directorio ya existente, responde `200` con el mismo cuerpo.

**Errores:** `400 INVALID_PATH`, `401`, `403`, `404 PARENT_NOT_FOUND` (sin `parents`), `409 ALREADY_EXISTS` (sin `parents`, o si existe un archivo con ese nombre), `409 NOT_A_DIRECTORY` (un padre es un archivo).

### 4.3 `DELETE /fs/rmdir?path=<ruta>`

Elimina un directorio **vacío**.

**Response 204** (sin cuerpo).

**Errores:** `400 INVALID_PATH` (incluye la raíz `/`), `401`, `403`, `404 NOT_FOUND`, `409 NOT_A_DIRECTORY`, `409 DIRECTORY_NOT_EMPTY`.

### 4.4 `DELETE /fs/rm?path=<ruta>`

Elimina un **archivo** del namespace y ordena la eliminación de sus bloques en los DataNodes (la eliminación física es asíncrona, vía heartbeat). Funciona también sobre archivos `UNDER_CONSTRUCTION` del propio usuario, lo que permite al cliente limpiar una subida fallida.

**Response 204** (sin cuerpo).

**Errores:** `400 INVALID_PATH`, `400 IS_A_DIRECTORY`, `401`, `403`, `404 NOT_FOUND`.

---

### 4.5 `POST /fs/chmod`

Cambia los permisos de un archivo o directorio. Solo puede hacerlo su dueño o un superusuario.

**Request**

```json
{
  "path": "/user/ana/datos.csv",
  "mode": "0644"
}
```

`mode` es octal de 3 o 4 dígitos entre `000` y `0777`.

**Response 200**

```json
{ "path": "/user/ana/datos.csv", "mode": "0644" }
```

**Errores:** `400 INVALID_REQUEST` (falta `path`/`mode` o el modo no es octal válido), `400 INVALID_PATH`, `401`, `403` (no es el dueño), `404 NOT_FOUND`.

### 4.6 Permisos de acceso (usuarios y grupos)

Cada archivo y directorio tiene un **dueño**, un **grupo** y permisos `rwx` para el dueño, el grupo y los demás, como en Linux.

| Operación | Permisos que se exigen |
|---|---|
| Llegar a cualquier ruta | `x` en todos los directorios del camino |
| `ls` de un directorio | `r` sobre el directorio |
| `locations` (get) | `r` sobre el archivo |
| `mkdir`, `allocate` (crear), `rmdir`, `rm` | `w` y `x` sobre el directorio que contiene la entrada |
| `chmod` | ser el dueño |

- Se aplica la regla del dueño, si no la del grupo (el usuario pertenece al grupo del archivo), si no la de los demás.
- **Valores por defecto:** los directorios nuevos son `0750` y los archivos `0640`; el grupo es el primer grupo del usuario. Un archivo recién creado es privado: otros usuarios no lo ven salvo que el dueño abra permisos con `chmod`.
- **Directorio personal:** en el primer login se crean `/user` (dueño `root`, `0755`) y `/user/<usuario>` (dueño el usuario, `0750`).
- **Superusuario:** el usuario `root` y los miembros del grupo `admin` pueden todo.
- Si el usuario no puede ver el directorio que contiene una ruta, recibe `403` exista o no lo que busca: no se revela su existencia.
- Quien abrió una subida (`allocate`) es el único que puede continuarla (`allocate`, `commit`, `finalize`), aunque otros tengan permiso de escritura en el directorio.

## 5. Escritura de archivos (write-once)

Flujo por archivo (ver `documentation/diagrams/01-put-escritura.puml`):

```text
para cada bloque i = 0..N-1:
    allocate(path, i, size)   -> blockId + pipeline
    WriteBlock (gRPC) al primer DataNode del pipeline
    commit(path, blockId, size, checksum)
finalize(path)
```

Reglas:

- Los archivos son *write-once*: una vez `COMPLETE` no se modifican.
- Los bloques se asignan **en orden** (`blockIndex` 0, 1, 2, ...). El `offset` de un bloque lo calcula el servidor como la suma de los tamaños de los bloques anteriores ya confirmados.
- El primer `allocate` de un path crea el archivo en estado `UNDER_CONSTRUCTION` y devuelve un `uploadId`. El cliente debe enviar ese `uploadId` en todas las llamadas siguientes de la misma subida. Así el servidor distingue la subida legítima de un segundo `put` concurrente al mismo path, que recibe `409 FILE_BEING_WRITTEN`.
- Si un bloque falla, el cliente puede repetir `allocate` con el **mismo** `blockIndex` y el mismo `uploadId`: el servidor descarta la asignación anterior (no confirmada) y devuelve un `blockId` y un `pipeline` nuevos.

### 5.1 `POST /files/allocate`

**Request**

```json
{
  "path": "/user/ana/datos.csv",
  "blockIndex": 0,
  "size": 8388608,
  "uploadId": ""
}
```

- `blockIndex`: índice del bloque, desde `0`.
- `size`: tamaño en bytes del bloque (el último puede ser menor). Debe ser mayor que `0`, con una excepción: un archivo **vacío** se sube con un único `allocate` de `blockIndex: 0`, `size: 0` y `uploadId` vacío, que crea el archivo y devuelve un `uploadId` sin `blockId` (`""`) ni `pipeline` (`[]`); después se llama a `finalize`.
- `uploadId`: vacío u omitido **solo** en el primer `allocate` (`blockIndex = 0`) de una subida; obligatorio después.

**Response 200**

```json
{
  "uploadId": "7f3c1d52-9a41-4a1e-b0c7-0d9d1f5b2a10",
  "blockId": "blk_7f3c1d52_0",
  "blockIndex": 0,
  "offset": 0,
  "size": 8388608,
  "pipeline": [
    { "id": "datanode-1", "host": "10.0.1.11", "port": 50051, "rack": "rack-a" },
    { "id": "datanode-2", "host": "10.0.1.12", "port": 50051, "rack": "rack-b" },
    { "id": "datanode-3", "host": "10.0.1.13", "port": 50051, "rack": "rack-b" }
  ]
}
```

- `pipeline`: lista **ordenada** de `REPLICATION_FACTOR` DataNodes vivos y distintos. El cliente escribe en el primero y le pasa los demás (`pipeline[1:]`) en la cabecera de `WriteBlock`.
- `host` y `port` son los que el DataNode anunció (`ADVERTISE_ADDR`). Son internos al cliente: nunca se muestran al usuario.

**Errores:** `400 INVALID_REQUEST`, `400 INVALID_PATH`, `401`, `403`, `404 PARENT_NOT_FOUND`, `409 ALREADY_EXISTS` (el path es un archivo `COMPLETE` o un directorio), `409 FILE_BEING_WRITTEN`, `409 BLOCK_OUT_OF_ORDER`, `503 INSUFFICIENT_DATANODES`, `503 NAMENODE_STANDBY`.

### 5.2 `POST /files/commit`

Confirma que el bloque quedó escrito en sus réplicas.

**Request**

```json
{
  "path": "/user/ana/datos.csv",
  "uploadId": "7f3c1d52-9a41-4a1e-b0c7-0d9d1f5b2a10",
  "blockId": "blk_7f3c1d52_0",
  "size": 8388608,
  "checksumSha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
}
```

**Response 200**

```json
{
  "blockId": "blk_7f3c1d52_0",
  "replicas": 3
}
```

`replicas` es el número de réplicas confirmadas por los DataNodes (`BlockReceived`) con el mismo checksum.

**Errores:** `400 INVALID_REQUEST`, `401`, `403`, `404 NOT_FOUND` (path o `blockId` desconocido), `409 FILE_BEING_WRITTEN` (`uploadId` no coincide), `409 BLOCK_NOT_REPLICATED`, `503 NAMENODE_STANDBY`.

### 5.3 `POST /files/finalize`

Cierra el archivo: pasa a `COMPLETE` y calcula su tamaño total.

**Request**

```json
{
  "path": "/user/ana/datos.csv",
  "uploadId": "7f3c1d52-9a41-4a1e-b0c7-0d9d1f5b2a10"
}
```

**Response 201**

```json
{
  "path": "/user/ana/datos.csv",
  "size": 20971520,
  "blocks": 3,
  "state": "COMPLETE"
}
```

**Errores:** `400 INVALID_REQUEST`, `401`, `403`, `404 NOT_FOUND`, `409 FILE_BEING_WRITTEN` (`uploadId` no coincide), `409 INCOMPLETE_FILE`, `409 ALREADY_EXISTS` (ya estaba `COMPLETE`), `503 NAMENODE_STANDBY`.

### 5.4 Limpieza de subidas fallidas

Si la subida no se puede completar, el cliente debe llamar a `DELETE /fs/rm?path=...` para no dejar un archivo `UNDER_CONSTRUCTION` huérfano (US-34).

### 5.5 Regla de consistencia estricta (RNF3)

Un bloque solo se acepta cuando **todas** sus réplicas existen con el mismo checksum:

- `commit` responde `409 BLOCK_NOT_REPLICATED` si el bloque no tiene `REPLICATION_FACTOR` réplicas confirmadas por `BlockReceived` con un checksum igual al enviado por el cliente.
- Hay una carrera posible entre el ACK del pipeline al cliente y el aviso `BlockReceived` de los DataNodes al NameNode. Por eso el NameNode espera brevemente y reintenta internamente antes de responder `409`.
- `commit` espera hasta `COMMIT_WAIT_MS` (por defecto 3000 ms) a que lleguen esos avisos antes de responder `409`, de modo que normalmente el cliente no ve la carrera. La respuesta indica cuántas réplicas hay (`"2 de 3"`).
- `commit` es idempotente: repetirlo con el mismo `blockId`, `size` y `checksumSha256` devuelve `200` de nuevo.
- `finalize` falla con `409 INCOMPLETE_FILE` si algún bloque está asignado pero sin confirmar. **No** vuelve a exigir las réplicas: si un DataNode cae después del `commit`, la re-replicación (US-56) restablece el factor y el `put` puede terminar.


---

## 6. Lectura de archivos

### 6.1 `GET /files/locations?path=<ruta>`

Devuelve, en orden, los bloques de un archivo y los DataNodes **vivos** que tienen cada uno. El cliente lee de cualquiera de ellos y, si uno falla (conexión o checksum), prueba el siguiente.

**Response 200**

```json
{
  "path": "/user/ana/datos.csv",
  "size": 20971520,
  "blocks": [
    {
      "blockId": "blk_7f3c1d52_0",
      "blockIndex": 0,
      "offset": 0,
      "size": 8388608,
      "checksumSha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
      "locations": [
        { "id": "datanode-1", "host": "10.0.1.11", "port": 50051, "rack": "rack-a" },
        { "id": "datanode-3", "host": "10.0.1.13", "port": 50051, "rack": "rack-b" }
      ]
    }
  ]
}
```

- Un archivo vacío devuelve `size: 0` y `blocks: []`.
- `locations` puede venir vacío si ningún DataNode vivo tiene el bloque; el cliente debe tratarlo como error de ese bloque.
- `locations` solo incluye DataNodes **vivos** cuya copia tiene el checksum confirmado del bloque. El orden rota por bloque (el bloque `i` empieza por la réplica `i mod n`) para que las descargas en paralelo se repartan entre los nodos; el cliente debe probarlas en el orden recibido.

**Errores:** `400 INVALID_PATH`, `401`, `403`, `404 NOT_FOUND`, `409 FILE_UNDER_CONSTRUCTION`, `503 NAMENODE_STANDBY`. Si `path` es un directorio se responde `400 IS_A_DIRECTORY`.

---

## 7. Extensiones previstas (no forman parte del contrato inicial)

Estos cambios se anunciarán al equipo con la etiqueta `contract-change` cuando se implementen:

- **US-84, token de bloque:** `allocate` y `locations` añadirán un campo `accessToken` por bloque, que el cliente reenvía al DataNode en `WriteBlock`/`ReadBlock`. Es un campo nuevo; los clientes que lo ignoren seguirán parseando la respuesta.

---

## 8. Ejemplos con `curl`

```bash
# Login
curl -s -X POST http://localhost:8080/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"ana","password":"secreto"}'

TOKEN=eyJhbGciOi...

# Crear directorio y listar
curl -s -X POST http://localhost:8080/fs/mkdir \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"path":"/user/ana/docs","parents":true}'

curl -s "http://localhost:8080/fs/ls?path=/user/ana" -H "Authorization: Bearer $TOKEN"

# Pedir un bloque
curl -s -X POST http://localhost:8080/files/allocate \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"path":"/user/ana/datos.csv","blockIndex":0,"size":8388608}'

# Borrar archivo
curl -s -X DELETE "http://localhost:8080/fs/rm?path=/user/ana/datos.csv" \
  -H "Authorization: Bearer $TOKEN"
```
