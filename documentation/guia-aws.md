# DFSha — Guía paso a paso de AWS Academy (documento temporal)

Cubre US-60, US-61 y US-62. Está dividida en **dos partes**:

- **Parte A — Infraestructura.** Se puede hacer **hoy**, sin código. Crear red, claves e instancias.
- **Parte B — Despliegue.** Necesita que `main` tenga el clúster funcionando en Docker local. Antes de empezarla hay un **punto de control** (sección 9) con la lista exacta de lo que debe estar mergeado.

---

## 1. Plan de direcciones (lo que vas a escribir en cada máquina)

Las IPs **privadas** las fijas tú al crear cada instancia, así quedan iguales para siempre y los `.env` se escriben una sola vez. Las **públicas** las asigna AWS y cambian cada vez que el lab se reinicia (se anotan en la sección 10).

| # | Nombre EC2 (tag `Name`) | Rol | Rack | **IP privada fija** | Puertos que expone |
|---|---|---|---|---|---|
| 1 | `dfsha-nn-active` | NameNode activo | — | `172.31.0.10` | 8080 (REST), 9000 (gRPC) |
| 2 | `dfsha-nn-standby` | NameNode standby | — | `172.31.0.11` | 8080, 9000 |
| 3 | `dfsha-dn-1` | DataNode | A | `172.31.0.21` | 50051 |
| 4 | `dfsha-dn-2` | DataNode | A | `172.31.0.22` | 50051 |
| 5 | `dfsha-dn-3` | DataNode | B | `172.31.0.23` | 50051 |
| 6 | `dfsha-dn-4` | DataNode | B | `172.31.0.24` | 50051 |

> **Antes de usar estas IPs comprueba la subred.** En la consola: VPC → **Subnets** → busca la subred de la VPC por defecto en la zona `us-east-1a` y mira su **IPv4 CIDR**. Si es `172.31.0.0/20`, las IPs de arriba sirven tal cual. Si es otra (p. ej. `172.31.16.0/20`), cambia el tercer octeto: `172.31.16.10`, `172.31.16.11`, `172.31.16.21`… y usa esos valores en todo el documento. **Todas las instancias van en esa misma subred.**

Con estas IPs, estos son los valores que se repiten en los `.env`:

```text
NAMENODE_ADDRS=172.31.0.10:9000,172.31.0.11:9000
PEER (visto desde nn-active)  = 172.31.0.11:9000
PEER (visto desde nn-standby) = 172.31.0.10:9000
```

---

# PARTE A — Infraestructura (se puede hacer ya)

## 2. Entrar al lab

1. AWS Academy → tu curso → **Modules** → **Learner Lab** → **Start Lab**.
2. Espera el círculo verde y pulsa **AWS** para abrir la consola.
3. Confirma la región arriba a la derecha: **N. Virginia (us-east-1)**. No la cambies entre sesiones.
4. Ten a mano la IP pública de cada integrante: [https://ifconfig.me](https://ifconfig.me). Se usa para el SSH.

Lo que debes saber de Academy:

- La sesión dura ~4 h. Al vencer, las instancias **se detienen** (no se borran); el disco se conserva.
- Al reiniciar el lab las **IPs públicas cambian**; las **privadas fijas** no.
- Normalmente **no** hay Elastic IPs. Si tu lab las permite, asigna una a cada nodo y te saltas la sección 15.

## 3. Par de claves

1. Consola → **EC2** → menú izquierdo **Network & Security → Key Pairs** → **Create key pair**.
2. Name: `dfsha-key` · Type: **RSA** · Format: **.pem** → **Create**.
3. Se descarga `dfsha-key.pem`. Guárdalo **fuera del repo** (p. ej. `C:\Users\<tu>\aws\dfsha-key.pem`) y compártelo al equipo por un canal privado, nunca por GitHub.
4. En PowerShell, restringe permisos (si no, `ssh` lo rechaza):
   ```powershell
   icacls "C:\Users\<tu>\aws\dfsha-key.pem" /inheritance:r /grant:r "$env:USERNAME:R"
   ```

## 4. Security group `dfsha-sg`

1. **EC2 → Network & Security → Security Groups → Create security group**.
2. Name: `dfsha-sg` · Description: `DFSha cluster` · VPC: la **default**.
3. **Inbound rules → Add rule**, una fila por cada línea:

| Type | Port | Source | Descripción |
|---|---|---|---|
| SSH | 22 | **My IP** (repite la regla por cada integrante) | administración |
| Custom TCP | 8080 | `0.0.0.0/0` | REST NameNode para el cliente |
| Custom TCP | 50051 | `0.0.0.0/0` | gRPC DataNode para el cliente |

4. **Outbound**: deja "All traffic".
5. **Create security group**.
6. Vuelve a abrirlo → **Edit inbound rules → Add rule** y añade las reglas internas. En **Source** escribe `dfsha-sg` y selecciónalo de la lista:

| Type | Port | Source | Descripción |
|---|---|---|---|
| Custom TCP | 9000 | `dfsha-sg` | gRPC NameNode (DataNodes y standby) |
| Custom TCP | 50051 | `dfsha-sg` | pipeline DataNode → DataNode |
| All ICMP - IPv4 | — | `dfsha-sg` | `ping` entre nodos |

7. **Save rules**.

## 5. Crear las 6 instancias

Repite estos pasos **6 veces**, una por fila de la tabla de la sección 1. Lo único que cambia entre una y otra es el **Name**, la **IP privada** y el **disco**.

1. **EC2 → Instances → Launch instances**.
2. **Name and tags → Name:** el nombre exacto de la tabla (p. ej. `dfsha-nn-active`).
3. **Application and OS Images:** **Ubuntu** → *Ubuntu Server 24.04 LTS (HVM), SSD Volume Type*, 64-bit (x86).
4. **Instance type:** `t3.small` (si el lab solo deja `t2.micro`, sirve para la demo con bloques de 8 MB).
5. **Key pair:** `dfsha-key`.
6. **Network settings → Edit:**
   - **VPC:** default.
   - **Subnet:** la de `us-east-1a` (la misma para las 6).
   - **Auto-assign public IP:** **Enable**.
   - **Firewall:** *Select existing security group* → `dfsha-sg`.
   - **Advanced network configuration** (desplegar) → **Primary IP:** la IP privada fija de la tabla (p. ej. `172.31.0.10`).
7. **Configure storage:** `8` GiB gp3 para los NameNodes, **`20` GiB** gp3 para los DataNodes.
8. **Advanced details** (desplegar) → al final, **User data:** pega el script de la sección 6.
9. **Launch instance**.
10. Repite con la siguiente fila.

Cuando las 6 estén en **Running**, comprueba en **Instances** que la columna *Private IPv4 address* coincide con la tabla.

## 6. User data (se pega en cada instancia al crearla)

Instala Docker y clona el repo en `/opt/dfsha/repo`. Si olvidaste pegarlo, entra por SSH y ejecútalo con `sudo bash`.

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
mkdir -p /opt/dfsha /opt/dfsha/certs /var/lib/dfsha/data
chown -R ubuntu:ubuntu /opt/dfsha /var/lib/dfsha
sudo -u ubuntu git clone https://github.com/FelipeOchoaL/DFSha.git /opt/dfsha/repo
```

Si el repo es **privado**, el `git clone` fallará: quita esa línea y clona después por SSH con un token o una deploy key.

## 7. Probar el acceso SSH

Desde tu PC, con la IP **pública** de cada instancia (columna *Public IPv4 address*):

```powershell
ssh -i "C:\Users\<tu>\aws\dfsha-key.pem" ubuntu@<IP_PUBLICA>
```

Dentro:

```bash
docker --version && docker compose version
ls /opt/dfsha/repo
ping -c 2 172.31.0.10      # desde cualquier DataNode: debe responder el NameNode
```

Para no escribir IPs, crea `C:\Users\<tu>\.ssh\config`:

```text
Host dfsha-nn-active
  HostName <IP_PUBLICA>
  User ubuntu
  IdentityFile C:/Users/<tu>/aws/dfsha-key.pem
```

Un bloque por nodo. Cuando cambien las públicas, solo actualizas este archivo.

## 8. Fin de la Parte A

Hasta aquí tienes: red, claves, 6 instancias con Docker y el repo clonado. **No sigas con la Parte B hasta pasar el punto de control.** Puedes hacer **End Lab** sin problema; todo se conserva.

---

# PUNTO DE CONTROL — ¿Está `main` listo para desplegar?

## 9. Lista de lo que debe estar mergeado en `main`

Desplegar en AWS sin esto solo sirve para depurar por SSH, que es lento. Verifica cada punto en GitHub (PR mergeado) y en local (`git checkout main && git pull`).

| ✓ | Historia | Quién | Cómo comprobarlo en local |
|---|---|---|---|
| ☐ | US-03 / US-04 contratos y stubs | Felipe | `services/gen/` y `client/gen/` tienen archivos; `cd services && go build ./...` pasa |
| ☐ | US-13 DataNode arranca | Carlos | `go run ./cmd/datanode` levanta gRPC en 50051 |
| ☐ | US-14 / US-15 heartbeat y BlockReceived | Carlos | en el log del NameNode aparece `dn1` registrado |
| ☐ | US-28 NameNode arranca | James | `go run ./cmd/namenode` levanta REST 8080 y gRPC 9000 |
| ☐ | US-25 / US-26 / US-27 REST | James | `curl` a `/fs/ls`, `/files/allocate`, `/files/locations` responde |
| ☐ | US-34 / US-35 / US-36 CLI | Felipe | `python -m client.main put/get/ls` contra el clúster local |
| ☐ | US-40 Dockerfiles | David | `docker build -f services/Dockerfile.namenode services/` y `...datanode` construyen |
| ☐ | US-41 docker-compose | David | `docker compose up` levanta 1 NN + 3 DN y `put`/`get` funcionan |
| ☐ | US-50 / US-51 / US-52 réplica 3/3 | David / Carlos / James | tras un `put`, cada bloque está en 3 volúmenes |
| ☐ | US-56 / US-57 re-replicación | David / Carlos | `docker compose stop datanode-3` → `get` OK → bloques vuelven a 3 copias |
| ☐ | US-59 compose con 4 DN + script de fallos | David | `scripts/e2e_replication.ps1` pasa en local |
| ☐ | Tag `hito2` en `main` | Felipe | `git tag` lo muestra |

**Regla:** cuando las 12 casillas estén marcadas y la prueba de US-59 pase en tu máquina con `docker compose`, pasas a la Parte B. Si falta alguna, lo que falla en AWS no se sabe si es red o código.

Para el **Hito 3** se repite el punto de control con: US-70–US-75 (HA), US-80–US-90 (seguridad) y el tag `hito3`.

---

# PARTE B — Despliegue (solo después del punto de control)

## 10. Tabla de IPs públicas (rellenar en cada sesión)

| Nodo | IP privada (fija) | IP pública hoy |
|---|---|---|
| dfsha-nn-active | 172.31.0.10 | |
| dfsha-nn-standby | 172.31.0.11 | |
| dfsha-dn-1 | 172.31.0.21 | |
| dfsha-dn-2 | 172.31.0.22 | |
| dfsha-dn-3 | 172.31.0.23 | |
| dfsha-dn-4 | 172.31.0.24 | |

## 11. NameNode activo — `dfsha-nn-active` (172.31.0.10)

```bash
ssh dfsha-nn-active
cd /opt/dfsha/repo && git checkout main && git pull
```

Crea `/opt/dfsha/namenode.env` con **exactamente** esto:

```bash
NODE_ID=nn1
NAMENODE_ROLE=active
REST_PORT=8080
GRPC_PORT=9000
ADVERTISE_ADDR=172.31.0.10:9000
PEER_NAMENODE_ADDR=172.31.0.11:9000
BLOCK_SIZE_MB=8
REPLICATION_FACTOR=3
DATA_DIR=/data
JWT_SECRET=CAMBIA-ESTO-por-una-cadena-larga-igual-en-los-dos-namenodes
```

Construye y arranca:

```bash
cd /opt/dfsha/repo/services
docker build -f Dockerfile.namenode -t dfsha/namenode .
docker rm -f namenode 2>/dev/null || true
docker run -d --name namenode --restart unless-stopped \
  --env-file /opt/dfsha/namenode.env \
  -p 8080:8080 -p 9000:9000 \
  -v /var/lib/dfsha/data:/data \
  dfsha/namenode
docker logs -f namenode      # Ctrl+C para salir
```

Verifica desde la misma máquina: `curl -s "http://localhost:8080/fs/ls?path=/"` (o el endpoint de salud que defina James).

## 12. NameNode standby — `dfsha-nn-standby` (172.31.0.11) — Hito 3

Igual que el anterior; en `/opt/dfsha/namenode.env` cambia solo estas líneas:

```bash
NODE_ID=nn2
NAMENODE_ROLE=standby
ADVERTISE_ADDR=172.31.0.11:9000
PEER_NAMENODE_ADDR=172.31.0.10:9000
```

Mismo `JWT_SECRET` que el activo. Arráncalo **después** del activo. En el Hito 2 puedes dejar esta instancia detenida.

## 13. DataNodes — `dfsha-dn-1` … `dfsha-dn-4`

En cada DataNode, `/opt/dfsha/datanode.env`. Las tres primeras líneas cambian por nodo; el resto es igual en los cuatro:

| Nodo | `NODE_ID` | `RACK` | `ADVERTISE_ADDR` |
|---|---|---|---|
| dfsha-dn-1 | `dn1` | `A` | `<IP_PUBLICA_dn-1>:50051` |
| dfsha-dn-2 | `dn2` | `A` | `<IP_PUBLICA_dn-2>:50051` |
| dfsha-dn-3 | `dn3` | `B` | `<IP_PUBLICA_dn-3>:50051` |
| dfsha-dn-4 | `dn4` | `B` | `<IP_PUBLICA_dn-4>:50051` |

Ejemplo para `dfsha-dn-1`:

```bash
NODE_ID=dn1
RACK=A
ADVERTISE_ADDR=<IP_PUBLICA_dn-1>:50051
GRPC_PORT=50051
NAMENODE_ADDRS=172.31.0.10:9000,172.31.0.11:9000
DATA_DIR=/data
BLOCK_ENCRYPTION_KEY=                      # Hito 3: 64 hex (32 bytes), igual en los 4
```

> `ADVERTISE_ADDR` es la dirección que el NameNode le entrega al **cliente** en `allocate` y `locations`. Como el cliente está fuera de AWS, debe ser la **IP pública**. Es la única línea que hay que tocar cuando el lab reinicia (sección 15).

Construye y arranca (igual en los cuatro):

```bash
ssh dfsha-dn-1
cd /opt/dfsha/repo && git checkout main && git pull
cd services
docker build -f Dockerfile.datanode -t dfsha/datanode .
docker rm -f datanode 2>/dev/null || true
docker run -d --name datanode --restart unless-stopped \
  --env-file /opt/dfsha/datanode.env \
  -p 50051:50051 \
  -v /var/lib/dfsha/data:/data \
  dfsha/datanode
docker logs --tail 20 datanode
```

Verifica en el NameNode que se registró: `docker logs namenode | grep -i dn1`.

**Orden de arranque:** nn-active → nn-standby → dn-1, dn-2, dn-3, dn-4.

## 14. Probar desde tu PC (US-62)

```powershell
$env:DFSHA_NAMENODES = "http://<IP_PUBLICA_nn-active>:8080,http://<IP_PUBLICA_nn-standby>:8080"
python -m client.main login felipe
python -m client.main mkdir -p /user/felipe
fsutil file createnew prueba.bin 20971520        # 20 MB = 3 bloques de 8 MB
python -m client.main put prueba.bin /user/felipe/prueba.bin
python -m client.main ls /user/felipe
python -m client.main get /user/felipe/prueba.bin descarga.bin
Get-FileHash prueba.bin, descarga.bin             # deben ser iguales
```

Verificar partición y réplica (en cada DataNode): `ls /var/lib/dfsha/data`. Cada `blockId` aparece en **exactamente 3** nodos y ningún nodo tiene los 3 bloques.

Prueba de fallo:

```bash
ssh dfsha-dn-3 "docker stop datanode"
```

Desde tu PC, `get` debe seguir funcionando. Al cabo de ~1 min, `ls /var/lib/dfsha/data` en otro DataNode muestra los bloques re-replicados. Luego `ssh dfsha-dn-3 "docker start datanode"`.

Failover (Hito 3): `ssh dfsha-nn-active "docker stop namenode"` → el siguiente comando del CLI responde desde el standby.

Guarda capturas: son evidencia para el informe y el video.

## 15. Cada vez que reinicias el lab

1. **Start Lab** → EC2 → **Instances** → selecciona las 6 → **Instance state → Start instance**.
2. Copia las nuevas IPs públicas a la tabla de la sección 10.
3. En **cada DataNode** actualiza `ADVERTISE_ADDR` en `/opt/dfsha/datanode.env` y reinicia:
   ```bash
   sudo sed -i "s#^ADVERTISE_ADDR=.*#ADVERTISE_ADDR=<NUEVA_IP_PUBLICA>:50051#" /opt/dfsha/datanode.env
   docker restart datanode
   ```
   Los NameNodes no cambian (usan privadas entre sí).
4. En tu PC: actualiza `DFSHA_NAMENODES` y `~/.ssh/config`.
5. Los contenedores tienen `--restart unless-stopped`: deberían estar arriba. Verifica con `docker ps`.

## 16. Redesplegar después de un merge en `main`

En el nodo afectado:

```bash
cd /opt/dfsha/repo && git pull
cd services && docker build -f Dockerfile.<namenode|datanode> -t dfsha/<namenode|datanode> .
docker rm -f <namenode|datanode>
# vuelve a ejecutar el docker run de la sección 11 o 13
```

El volumen `/var/lib/dfsha/data` no se toca: bloques y edit log sobreviven. Esto es lo que deben automatizar `deploy/aws/namenode.sh` y `deploy/aws/datanode.sh` (US-61).

## 17. Hito 3: TLS

Cuando exista `deploy/certs/gen_certs.sh` (US-86):

1. Genera la CA y un certificado por nodo con su **IP privada y pública en el SAN**.
2. Cópialos: `scp -i dfsha-key.pem ca.crt dn1.crt dn1.key ubuntu@<IP>:/opt/dfsha/certs/`.
3. Añade al `docker run` `-v /opt/dfsha/certs:/certs:ro` y al `.env`: `TLS_ENABLED=true`, `TLS_CERT=/certs/dn1.crt`, `TLS_KEY=/certs/dn1.key`, `TLS_CA=/certs/ca.crt`.
4. En el cliente: `DFSHA_NAMENODES=https://...` y `DFSHA_CA_CERT=ruta\ca.crt`.

## 18. Problemas frecuentes

| Síntoma | Causa probable | Revisa |
|---|---|---|
| `ssh: connect timed out` | lab detenido o IP pública vieja | sección 15 |
| `Permission denied (publickey)` | usuario o permisos del `.pem` | usuario `ubuntu`; `icacls` de la sección 3 |
| `put` falla al escribir el bloque, pero `ls` funciona | `ADVERTISE_ADDR` del DataNode es privada o 50051 cerrado | `.env` del DN y regla 50051 en `dfsha-sg` |
| DataNode no aparece registrado | `NAMENODE_ADDRS` mal o 9000 no abierto dentro del SG | `docker logs datanode`; regla 9000 origen `dfsha-sg` |
| `allocate`: "no hay suficientes DataNodes" | menos de 3 DN vivos | `docker ps` en cada DN |
| No pude fijar la IP privada al crear | la IP ya estaba usada o fuera del CIDR | sección 1: verifica el CIDR de la subred |
| Tras reiniciar el lab todo falla | IPs públicas nuevas | sección 15 |

## 19. Apagar

- Al terminar: **End Lab**. Detiene todo; no hace falta apagar a mano.
- No uses **Terminate** (borra instancia y disco). Para apagar una sola, **Stop**.
- Al cierre del semestre, con video y nota entregados, se pueden terminar las 6.
