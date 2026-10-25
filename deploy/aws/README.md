# Despliegue en las EC2 que ya existen

No crea instancias. La red, el security group `dfsha-sg` y las 6 máquinas están en `documentation/guia-aws.md`.

| EC2 | IP privada | Archivo de ejemplo |
|---|---|---|
| dfsha-nn-active | 172.31.0.10 | `env/nn-active.env.example` |
| dfsha-nn-standby | 172.31.0.11 | `env/nn-standby.env.example` |
| dfsha-dn-1 (rack A) | 172.31.0.21 | `env/dn-1.env.example` |
| dfsha-dn-2 (rack A) | 172.31.0.22 | `env/dn-2.env.example` |
| dfsha-dn-3 (rack B) | 172.31.0.23 | `env/dn-3.env.example` |
| dfsha-dn-4 (rack B) | 172.31.0.24 | `env/dn-4.env.example` |

Estos scripts despliegan **cuando `main` ya compila** (Dockerfiles reales y binarios de Carlos y James). Hoy `services/Dockerfile.*` siguen vacíos: `docker build` va a fallar hasta ese merge. TLS está en `false` a propósito; los certificados son el Hito 3.

## En cada EC2

```bash
cd /opt/dfsha/repo && git checkout main && git pull
# NameNode activo:
cp deploy/aws/env/nn-active.env.example /opt/dfsha/node.env
# DataNode: copia el ejemplo de ese nodo y cambia <IP_PUBLICA>
chmod +x deploy/aws/*.sh
ENV_FILE=/opt/dfsha/node.env deploy/aws/namenode.sh    # o datanode.sh
```

`git pull` trae `main`. El volumen `/var/lib/dfsha/data` no se borra al redesplegar.

## Qué del trabajo de David sí quedó y qué no

El zip usaba la red `10.0.1.10`, TLS obligatorio, `users.json` y variables que el código Go todavía no lee. Eso se reescribió a las IP de la guía.

| Historia | Dónde | Estado |
|---|---|---|
| US-60 EC2 | Ya hecha en la consola | Hecha. No repetir. |
| US-61 scripts | `namenode.sh`, `datanode.sh`, `env/` | Lista para cuando existan los Dockerfiles |
| US-86 certificados | `deploy/certs/gen_certs.sh` | Script listo; no generar ni subir claves hasta el Hito 3 |
| US-22, US-24, US-50, US-56, US-80, US-84 | Código Go de David en el zip | No se copió. Depende del NameNode de James y chocaría con los archivos vacíos del repo |

Cuando James y Carlos estén en `dev`, el Go de David (placement, registro de DataNodes, auth) se integra en su propia rama, no dentro de `deploy/`.
