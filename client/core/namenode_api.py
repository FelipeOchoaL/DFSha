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

    def allocate(self, path: str, block_index: int, offset: int, size: int) -> dict[str, Any]:
        """Devuelve blockId, offset, size y pipeline [{id, host, port, rack}, ...]."""
        return self._request("POST", "/files/allocate", body={
            "path": path, "blockIndex": block_index, "offset": offset, "size": size})

    def commit(self, path: str, block_id: str, size: int, checksum_sha256: str,
               replica_ids: Sequence[str] = ()) -> dict[str, Any]:
        body = {"path": path, "blockId": block_id, "size": size,
                "checksumSha256": checksum_sha256}
        if replica_ids:
            body["replicaIds"] = list(replica_ids)
        return self._request("POST", "/files/commit", body=body)

    def finalize(self, path: str) -> dict[str, Any]:
        return self._request("POST", "/files/finalize", body={"path": path})

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
                resp = self._http.request(method, f"{base}{route}", params=params, json=body,
                                          headers=headers, timeout=self._timeout,
                                          verify=self._verify)
            except requests.RequestException as exc:
                last_error = NameNodeUnavailableError("No se pudo contactar al NameNode")
                last_error.__cause__ = exc
                continue

            if resp.status_code == 503:
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
        if resp.status_code == 503:
            return NameNodeStandbyError(message, resp.status_code, code)
        return NameNodeError(message, resp.status_code, code)
