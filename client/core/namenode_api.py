"""Cliente REST del NameNode: un método por endpoint (US-33) con failover (US-73).

Rutas, métodos HTTP, Bearer y formato de error salen del backlog. Los nombres de
campos JSON (blockIndex, checksumSha256, parents...) y que los DELETE/GET lleven
`path` por query string son SUPOSICIONES: verifícalas contra
documentation/api-rest.md (US-05) y ajusta aquí. Para leer respuestas usa `pick`,
que acepta camelCase y snake_case.

Failover: se intenta la URL activa; ante timeout, error de conexión o 503
(standby) se prueba la siguiente. La URL que respondió se recuerda en
`active_url` para que la sesión la guarde.
"""
from __future__ import annotations

import time
from typing import Any, Sequence

import requests

from client.core.errors import (
    NameNodeError,
    NameNodeStandbyError,
    NameNodeUnavailableError,
)


def pick(data: dict[str, Any], *keys: str, default: Any = None) -> Any:
    """Devuelve el primer valor presente entre varias claves (camelCase / snake_case)."""
    for key in keys:
        if key in data:
            return data[key]
    return default


class NameNodeAPI:
    def __init__(self, urls: Sequence[str], token: str | None = None,
                 timeout: float = 10.0, ca_cert: str | None = None,
                 preferred: str | None = None):
        if not urls:
            raise ValueError("Se necesita al menos una URL de NameNode")
        self._urls = [u.rstrip("/") for u in urls]
        # La última URL que funcionó va primero.
        if preferred and preferred.rstrip("/") in self._urls:
            self._urls.remove(preferred.rstrip("/"))
            self._urls.insert(0, preferred.rstrip("/"))
        self.active_url: str = self._urls[0]
        self._token = token
        self._timeout = timeout
        self._verify: bool | str = ca_cert if ca_cert else True
        self._http = requests.Session()

    # ---------- auth ----------

    def login(self, username: str, password: str) -> dict[str, Any]:
        return self._request("POST", "/auth/login",
                             body={"username": username, "password": password}, auth=False)

    # ---------- namespace ----------

    def ls(self, path: str) -> dict[str, Any]:
        return self._request("GET", "/fs/ls", params={"path": path})

    def mkdir(self, path: str, parents: bool = False) -> dict[str, Any]:
        return self._request("POST", "/fs/mkdir", body={"path": path, "parents": parents})

    def rmdir(self, path: str) -> dict[str, Any]:
        return self._request("DELETE", "/fs/rmdir", params={"path": path})

    def rm(self, path: str) -> dict[str, Any]:
        return self._request("DELETE", "/fs/rm", params={"path": path})

    # ---------- escritura ----------

    def allocate(self, path: str, block_index: int, offset: int, size: int,
                 *, upload_id: str | None = None) -> dict[str, Any]:
        """Abre una subida o continúa la identificada por upload_id."""
        body = {"path": path, "blockIndex": block_index, "offset": offset, "size": size}
        if upload_id is not None:
            body["uploadId"] = upload_id
        return self._request("POST", "/files/allocate", body=body)

    def commit(self, path: str, block_id: str, size: int, checksum_sha256: str,
               replica_ids: Sequence[str] = (), *, upload_id: str) -> dict[str, Any]:
        body = {"path": path, "uploadId": upload_id, "blockId": block_id, "size": size,
                "checksumSha256": checksum_sha256}
        if replica_ids:
            body["replicaIds"] = list(replica_ids)
        return self._request("POST", "/files/commit", body=body)

    def finalize(self, path: str, *, upload_id: str) -> dict[str, Any]:
        return self._request("POST", "/files/finalize", body={"path": path, "uploadId": upload_id})

    # ---------- lectura ----------

    def locations(self, path: str) -> dict[str, Any]:
        return self._request("GET", "/files/locations", params={"path": path})

    # ---------- internos ----------
    def _request(self, method: str, route: str, *, params: dict | None = None,
                 body: dict | None = None, auth: bool = True) -> dict[str, Any]:
        headers = {}
        if auth and self._token:
            headers["Authorization"] = f"Bearer {self._token}"
        last_error: Exception | None = None
        # Empieza por la activa y sigue con las demás en orden.
        order = [self.active_url] + [u for u in self._urls if u != self.active_url]
        for base in order:
            try:
                resp = self._send(method, f"{base}{route}", params, body, headers)
            except requests.RequestException as exc:
                last_error = NameNodeUnavailableError("No se pudo contactar al NameNode")
                last_error.__cause__ = exc
                continue
            if resp.status_code == 503 and _is_standby(resp):
                # Solo NAMENODE_STANDBY (o un 503 sin código, p. ej. de un proxy) significa
                # "prueba el siguiente". INSUFFICIENT_DATANODES y NAMENODE_RECOVERING los
                # devuelve el NameNode ACTIVO: cambiar de NameNode no los arregla.
                last_error = self._error_from(resp)
                continue
            self.active_url = base
            if resp.status_code >= 400:
                raise self._error_from(resp)
            if resp.status_code == 204 or not resp.content:
                return {}
            try:
                return resp.json()
            except ValueError:
                raise NameNodeError("Respuesta del NameNode no es JSON válido",
                                    resp.status_code) from None
        assert last_error is not None
        if isinstance(last_error, NameNodeStandbyError):
            raise NameNodeUnavailableError("Ningún NameNode está activo (todos responden standby)")
        raise last_error

    def _send(self, method: str, url: str, params, body, headers) -> requests.Response:
        """Hace la petición. Si el NameNode recién promovido responde NAMENODE_RECOVERING
        (espera los block reports de los DataNodes) reintenta en la MISMA URL, esperando
        lo que indica Retry-After, hasta RECOVERY_RETRIES veces."""
        for attempt in range(RECOVERY_RETRIES + 1):
            resp = self._http.request(method, url, params=params, json=body, headers=headers,
                                      timeout=self._timeout, verify=self._verify)
            if resp.status_code != 503 or _error_code(resp) != CODE_RECOVERING:
                return resp
            if attempt < RECOVERY_RETRIES:
                _sleep(_retry_after(resp))
        return resp

    @staticmethod
    def _error_from(resp: requests.Response) -> NameNodeError:
        message, code = resp.reason or "Error", None
        try:
            payload = resp.json()
            if isinstance(payload, dict):
                message = payload.get("error", message)
                code = payload.get("code")
        except ValueError:
            pass
        if resp.status_code == 503 and _is_standby(resp):
            return NameNodeStandbyError(message, resp.status_code, code)
        return NameNodeError(message, resp.status_code, code)


# ---------- ayudas de los 503 ----------

CODE_RECOVERING = "NAMENODE_RECOVERING"
CODE_NO_DATANODES = "INSUFFICIENT_DATANODES"
RECOVERY_RETRIES = 30  # con Retry-After de 1 s: hasta ~30 s tras un failover
_sleep = time.sleep    # sustituible en las pruebas


def _error_code(resp: requests.Response) -> str | None:
    """El campo `code` del cuerpo de error del NameNode, si lo hay."""
    try:
        payload = resp.json()
    except ValueError:
        return None
    return payload.get("code") if isinstance(payload, dict) else None


def _is_standby(resp: requests.Response) -> bool:
    """Un 503 significa 'este NameNode es standby' salvo que traiga otro código del activo."""
    return _error_code(resp) not in (CODE_RECOVERING, CODE_NO_DATANODES)


def _retry_after(resp: requests.Response) -> float:
    try:
        value = float(getattr(resp, "headers", {}).get("Retry-After", 1))
    except (TypeError, ValueError):
        value = 1.0
    return min(max(value, 0.1), 5.0)