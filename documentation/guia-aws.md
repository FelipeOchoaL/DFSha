# DFSha — Guía de despliegue en AWS Academy (documento temporal)

Cubre US-60, US-61 y US-62 del backlog. Sirve para el primer despliegue (Hito 2) y para los redespliegues de los Hitos 3 y 4.

Puertos usados en todo el documento (coinciden con `.env.example` y `client/config.py`):

| Servicio | Puerto | Quién se conecta |
|---|---|---|
| NameNode REST | 8080 | Cliente (desde fuera de AWS) |
| NameNode gRPC | 9000 | DataNodes y el otro NameNode (solo dentro de la VPC) |
| DataNode gRPC | 50051 | Cliente (desde fuera) y otros DataNodes (pipeline) |
| SSH | 22 | Solo el equipo |

---

## 1. Instancias que se crean

6 instancias en total, todas en la **misma VPC y la misma subred**.

| # | Nombre (tag `Name`) | Rol | Rack lógico | Variables clave |
|---|---|---|---|---|
| 1 | `dfsha-nn-active` | NameNode activo | — | `NAMENODE_ROLE=active` |
| 2 | `dfsha-nn-standby` | NameNode standby (Hito 3) | — | `NAMENODE_ROLE=standby` |
| 3 | `dfsha-dn-1` | DataNode | A | `NODE_ID=dn1`, `RACK=A` |
| 4 | `dfsha-dn-2` | DataNode | A | `NODE_ID=dn2`, `RACK=A` |
| 5 | `dfsha-dn-3` | DataNode | B | `NODE_ID=dn3`, `RACK=B` |
| 6 | `dfsha-dn-4` | DataNode | B | `NODE_ID=dn4`, `RACK=B` |

Para el Hito 2 puedes crear solo 5 (sin `nn-standby`) y agregarla en el Hito 3. Es más simple crear las 6 de una vez y dejar la standby apagada hasta que se necesite.

Configuración común de todas:

- **AMI:** Ubuntu Server 24.04 LTS (64-bit x86).
- **Tipo:** `t3.small` para NameNodes, `t3.small` o `t3.medium` para DataNodes (si el lab solo permite `t2.micro`, sirve para la demo con bloques de 8 MB).
- **Disco:** 8 GB para NameNodes, **20 GB** para DataNodes (ahí viven los bloques).
- **Par de claves:** uno solo para todo el equipo, `dfsha-key.pem`.
- **Security group:** `dfsha-sg` (ver sección 3).

---

## 2. Antes de empezar (una sola vez)

1. Entra a AWS Academy → Learner Lab → **Start Lab**. Espera a que el círculo esté verde y abre **AWS** (consola).
2. Región: la que te dé el lab (normalmente `us-east-1`). **No la cambies** entre sesiones.
3. Ten a mano la IP pública de cada integrante (`https://ifconfig.me`). Se usa para el SSH.

Cosas de AWS Academy que afectan al proyecto:

- La sesión dura unas 4 horas. Al vencer, las instancias **se detienen** (no se borran). Los datos del disco se conservan.
- Al volver a iniciar, **las IPs públicas cambian**. Las IPs **privadas** se mantienen. Por eso todo lo interno (NameNode ↔ DataNode) usa IPs privadas.
- No se pueden crear Elastic IPs en la mayoría de labs; si tu lab lo permite, asigna una a cada nodo y te ahorras la sección 8.
- El rol `LabRole` es el único disponible; no necesitas IAM para este proyecto.

---

## 3. Paso a paso: red y seguridad

### 3.1 Par de claves

1. EC2 → **Key Pairs** → **Create key pair**.
2. Nombre `dfsha-key`, tipo RSA, formato `.pem`.
3. Se descarga `dfsha-key.pem`. Guárdalo fuera del repo y compártelo al equipo por un canal privado (no por GitHub).
4. En Windows, restringe permisos para que `ssh` lo acepte:
   ```powershell
   icacls dfsha-key.pem /inheritance:r /grant:r "$env:USERNAME:R"
   ```

### 3.2 Security group `dfsha-sg`

EC2 → **Security Groups** → **Create security group**. VPC: la por defecto.

Reglas de entrada (**Inbound**):

| Tipo | Puerto | Origen | Para qué |
|---|---|---|---|
| SSH | 22 | IP de cada integrante (`x.x.x.x/32`, una regla por persona) | administración |
| Custom TCP | 8080 | `0.0.0.0/0` (o las IPs del equipo) | REST del NameNode para el cliente |
| Custom TCP | 50051 | `0.0.0.0/0` (o las IPs del equipo) | gRPC DataNode para el cliente |
| Custom TCP | 9000 | **el propio grupo** `dfsha-sg` | gRPC NameNode, solo interno |
| Custom TCP | 50051 | **el propio grupo** `dfsha-sg` | pipeline DataNode → DataNode |
| All ICMP – IPv4 | — | `dfsha-sg` | `ping` entre nodos para diagnosticar |

Para la regla "el propio grupo": en **Source** escribe el nombre `dfsha-sg` y selecciónalo. Hay que crear el grupo primero y editarlo después para añadir esas reglas.

Salida (**Outbound**): dejar "All traffic" (lo necesita `apt`, `docker pull` y `git clone`).

Para el video y la nota puedes abrir 8080 y 50051 a `0.0.0.0/0`; para dejarlo mejor, restringe a las IPs del equipo.

---

## 4. Paso a paso: crear cada instancia

Repite esto 6 veces (o usa **Number of instances = 4** para los DataNodes y luego renombra).

1. EC2 → **Instances** → **Launch instances**.
2. **Name:** según la tabla de la sección 1.
3. **AMI:** Ubuntu Server 24.04 LTS.
4. **Instance type:** `t3.small` (o lo que permita el lab).
5. **Key pair:** `dfsha-key`.
6. **Network settings** → **Edit**:
   - VPC por defecto, subred por defecto (la misma para todas).
   - **Auto-assign public IP: Enable**.
   - **Select existing security group:** `dfsha-sg`.
7. **Storage:** 8 GB (NameNodes) o 20 GB (DataNodes), gp3.
8. **Advanced details → User data:** pega el script de la sección 5 para no instalar Docker a mano.
9. **Launch instance**.
10. Cuando esté `Running`, anota en la tabla de la sección 7 su **IP privada** y su **IP pública**.

---

## 5. User data (instala Docker y clona el repo al arrancar)

Pégalo en *User data* al crear cada instancia. Si ya la creaste, ejecútalo por SSH con `sudo bash`.

```bash
#!/bin/bash
set -e
apt-get update -y
apt-get install -y ca-certificates curl git
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list
apt-get update -y
apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
usermod -aG docker ubuntu
mkdir -p /opt/dfsha /var/lib/dfsha/data
chown -R ubuntu:ubuntu /opt/dfsha /var/lib/dfsha
sudo -u ubuntu git clone https://github.com/FelipeOchoaL/DFSha.git /opt/dfsha/repo
```

Si el repo es privado, quita la última línea y clona por SSH después de subir una deploy key, o usa un token.

Verificación por SSH:

```bash
docker --version
docker compose version
ls /opt/dfsha/repo
```

---

## 6. Conectarse por SSH

Desde tu PC:

```powershell
ssh -i dfsha-key.pem ubuntu@<IP_PUBLICA>
```

Si sale `Permission denied (publickey)`, el usuario de Ubuntu es `ubuntu` (no `ec2-user`) o el `.pem` tiene permisos abiertos (sección 3.1).

Consejo: crea un bloque por nodo en `~/.ssh/config` para no escribir la IP:

```text
Host dfsha-nn-active
  HostName <IP_PUBLICA>
  User ubuntu
  IdentityFile C:/ruta/dfsha-key.pem
```

Cuando cambien las IPs (sección 8), solo actualizas este archivo.

---

## 7. Tabla de IPs (rellenar)

| Nodo | IP privada (fija) | IP pública (cambia) |
|---|---|---|
| dfsha-nn-active | 172.31.x.x | |
| dfsha-nn-standby | 172.31.x.x | |
| dfsha-dn-1 | 172.31.x.x | |
| dfsha-dn-2 | 172.31.x.x | |
| dfsha-dn-3 | 172.31.x.x | |
| dfsha-dn-4 | 172.31.x.x | |

Las privadas se escriben una sola vez. Las públicas se actualizan en cada sesión del lab.

---

## 8. Paso a paso por tipo de nodo

Todo se ejecuta dentro de la instancia por SSH. Los scripts `deploy/aws/namenode.sh` y `deploy/aws/datanode.sh` (US-61) deben hacer exactamente esto; mientras no existan, se hace a mano.

### 8.1 NameNode activo (`dfsha-nn-active`)

1. Entra por SSH.
2. Actualiza el código:
   ```bash
   cd /opt/dfsha/repo && git pull
   ```
3. Crea `/opt/dfsha/namenode.env` (no va en el repo):
   ```bash
   NODE_ID=nn1
   NAMENODE_ROLE=active
   REST_PORT=8080
   GRPC_PORT=9000
   ADVERTISE_ADDR=<IP_PRIVADA_NN_ACTIVE>:9000
   PEER_NAMENODE_ADDR=<IP_PRIVADA_NN_STANDBY>:9000
   BLOCK_SIZE_MB=8
   REPLICATION_FACTOR=3
   JWT_SECRET=<secreto largo, igual en los dos NameNodes>
   DATA_DIR=/data
   ```
4. Construye y ejecuta:
   ```bash
   cd /opt/dfsha/repo/services
   docker build -f Dockerfile.namenode -t dfsha/namenode .
   docker rm -f namenode 2>/dev/null || true
   docker run -d --name namenode --restart unless-stopped \
     --env-file /opt/dfsha/namenode.env \
     -p 8080:8080 -p 9000:9000 \
     -v /var/lib/dfsha/data:/data \
     dfsha/namenode
   ```
   El volumen guarda el edit log y el snapshot (US-70); así un reinicio no pierde metadatos.
5. Verifica:
   ```bash
   docker logs -f namenode
   curl -s http://localhost:8080/fs/ls?path=/   # o el endpoint de salud que defina James
   ```

### 8.2 NameNode standby (`dfsha-nn-standby`) — Hito 3

Igual que el activo, cambiando en el `.env`:

```bash
NODE_ID=nn2
NAMENODE_ROLE=standby
ADVERTISE_ADDR=<IP_PRIVADA_NN_STANDBY>:9000
PEER_NAMENODE_ADDR=<IP_PRIVADA_NN_ACTIVE>:9000
```

Mismo `JWT_SECRET`. Arranca después del activo para que sincronice el edit log.

### 8.3 DataNodes (`dfsha-dn-1` … `dfsha-dn-4`)

Repite en cada uno cambiando `NODE_ID`, `RACK` y `ADVERTISE_ADDR`.

1. Entra por SSH y actualiza: `cd /opt/dfsha/repo && git pull`.
2. Crea `/opt/dfsha/datanode.env`:
   ```bash
   NODE_ID=dn1                 # dn1, dn2, dn3, dn4
   RACK=A                      # A para dn1 y dn2, B para dn3 y dn4
   GRPC_PORT=50051
   ADVERTISE_ADDR=<IP_PUBLICA_DE_ESTE_DN>:50051
   NAMENODE_ADDRS=<IP_PRIVADA_NN_ACTIVE>:9000,<IP_PRIVADA_NN_STANDBY>:9000
   DATA_DIR=/data
   BLOCK_ENCRYPTION_KEY=<clave AES de 32 bytes en hex, igual en los 4>   # Hito 3
   ```
   Detalle importante: `ADVERTISE_ADDR` es lo que el NameNode le entrega al **cliente** en `allocate`/`locations`. Como el cliente está fuera de AWS, tiene que ser la **IP pública**. Los DataNodes entre sí también usarán esa IP (el tráfico sale y vuelve a entrar por el security group; la regla 50051 desde `0.0.0.0/0` lo permite). Si más adelante todo el tráfico de datos es interno, cámbialo a la privada.
3. Construye y ejecuta:
   ```bash
   cd /opt/dfsha/repo/services
   docker build -f Dockerfile.datanode -t dfsha/datanode .
   docker rm -f datanode 2>/dev/null || true
   docker run -d --name datanode --restart unless-stopped \
     --env-file /opt/dfsha/datanode.env \
     -p 50051:50051 \
     -v /var/lib/dfsha/data:/data \
     dfsha/datanode
   ```
4. Verifica que se registró: en el NameNode, `docker logs namenode | grep -i register` debe mostrar `dn1`.

### 8.4 Orden de arranque

1. `nn-active`
2. `nn-standby` (si aplica)
3. `dn-1` … `dn-4`

Si un DataNode arranca antes que el NameNode, debe reintentar `Register` solo (US-14), así que el orden no es crítico, pero este evita ruido en los logs.

---

## 9. Probar desde tu PC (US-62)

```powershell
$env:DFSHA_NAMENODES = "http://<IP_PUBLICA_NN_ACTIVE>:8080,http://<IP_PUBLICA_NN_STANDBY>:8080"
python -m client.main login
python -m client.main mkdir /user/felipe
fsutil file createnew prueba.bin 20971520      # 20 MB = 3 bloques de 8 MB
python -m client.main put prueba.bin /user/felipe/prueba.bin
python -m client.main ls /user/felipe
python -m client.main get /user/felipe/prueba.bin descarga.bin
Get-FileHash prueba.bin, descarga.bin
```

Verificar partición y réplica en los nodos:

```bash
# en cada DataNode
ls /var/lib/dfsha/data
```

Cada `blockId` debe aparecer en exactamente 3 DataNodes y ninguno debe tener los 3 bloques.

Prueba de fallo:

```bash
# en dfsha-dn-3
docker stop datanode
```

Desde tu PC, `get` debe seguir funcionando. Al cabo de un minuto, `ls /var/lib/dfsha/data` en el DataNode que no tenía esos bloques debe mostrarlos (re-replicación, US-57). Luego `docker start datanode`.

Prueba de failover (Hito 3): `docker stop namenode` en `nn-active`; el siguiente comando del CLI debe responder desde el standby.

---

## 10. Cada vez que reinicias el lab

Las instancias quedan detenidas y las IPs públicas cambian. Checklist:

1. **Start Lab** → consola EC2 → selecciona las 6 → **Instance state → Start**.
2. Anota las nuevas IPs públicas en la tabla de la sección 7.
3. En **cada DataNode**: edita `ADVERTISE_ADDR` en `/opt/dfsha/datanode.env` con la nueva IP pública y reinicia:
   ```bash
   docker restart datanode
   ```
   Los NameNodes no necesitan cambio (usan IPs privadas entre sí).
4. En tu PC: actualiza `DFSHA_NAMENODES` y `~/.ssh/config`.
5. Los contenedores tienen `--restart unless-stopped`, así que arrancan solos; verifica con `docker ps`.

Para evitar el paso 3, opción recomendada si te lo permite el lab: una **Elastic IP** por DataNode y por NameNode.

---

## 11. Redesplegar después de un merge

En el nodo afectado:

```bash
cd /opt/dfsha/repo && git pull
cd services && docker build -f Dockerfile.<rol> -t dfsha/<rol> .
docker rm -f <rol> && docker run ...   # mismo comando de la sección 8
```

El volumen `/var/lib/dfsha/data` no se toca, así que los bloques y el edit log sobreviven al redespliegue. Esto es lo que deben automatizar `deploy/aws/namenode.sh` y `deploy/aws/datanode.sh`.

---

## 12. Hito 3: TLS y certificados

Cuando exista `deploy/certs/gen_certs.sh` (US-86):

1. Genera la CA y un certificado por nodo **con la IP privada y la pública en el SAN** (si no, la verificación TLS falla).
2. Copia a cada nodo con `scp`:
   ```powershell
   scp -i dfsha-key.pem ca.crt dn1.crt dn1.key ubuntu@<IP>:/opt/dfsha/certs/
   ```
3. Monta la carpeta en el contenedor: `-v /opt/dfsha/certs:/certs:ro` y añade al `.env` `TLS_ENABLED=true`, `TLS_CERT=/certs/dn1.crt`, `TLS_KEY=/certs/dn1.key`, `TLS_CA=/certs/ca.crt`.
4. En el cliente: `DFSHA_NAMENODES=https://...` y `DFSHA_CA_CERT=ruta/ca.crt`.

---

## 13. Problemas frecuentes

| Síntoma | Causa probable | Qué revisar |
|---|---|---|
| `ssh: connect timed out` | el lab está detenido o la IP cambió | sección 10 |
| `Permission denied (publickey)` | usuario o permisos del `.pem` | usuario `ubuntu`, `icacls` |
| El cliente conecta al NameNode pero `put` falla en el bloque | `ADVERTISE_ADDR` del DataNode es privada o el puerto 50051 está cerrado | `.env` del DN y `dfsha-sg` |
| DataNode no aparece registrado | `NAMENODE_ADDRS` mal o puerto 9000 no abierto dentro del SG | `docker logs datanode` |
| `allocate` devuelve error de "no hay suficientes DataNodes" | menos de 3 DN vivos | `docker ps` en cada DN |
| Después de reiniciar el lab todo falla | IPs públicas nuevas | sección 10 |
| Disco lleno en un DataNode | muchas pruebas de `put` | `rm` desde el CLI o borrar `/var/lib/dfsha/data` solo en pruebas |

---

## 14. Costos y apagado

- Al terminar cada sesión: **End Lab** detiene todo; no hace falta apagar a mano.
- No uses **Terminate**: borra la instancia y el disco. Para "apagar" usa **Stop**.
- Al final del semestre, cuando ya esté el video y la nota, se pueden terminar las 6 instancias.
