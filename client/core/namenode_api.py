# Cliente REST del NameNode (login, ls, mkdir, rm, allocate, commit, finalize, locations)
# con failover: si el activo no responde, reintenta contra el standby.
"""Cliente REST del NameNode: un método por endpoint (US-33).

ATENCIÓN: rutas, métodos HTTP, Bearer y el formato de error salen del backlog.
Los nombres de campos JSON (blockIndex, checksumSha256, parents...) y que
los DELETE/GET lleven `path` por query string son SUPOSICIONES: verifícalas
contra documentation/api-rest.md (US-05) y ajusta aquí.
"""
from __future__ import annotations

from typing import Any, Sequence

import requests

from client.core.errors import (
    NameNodeError,
    NameNodeStandbyError,
    NameNodeUnavailableError,
)


class NameNodeAPI:
    def __init__(self, urls: Sequence[str], token: str | None = None,
                 timeout: float = 10.0, ca_cert: str | None = None):
        if not urls:
            raise ValueError("Se necesita al menos una URL de NameNode")
        self._urls = [u.rstrip("/") for u in urls]
        self._base = self._urls[0]  # US-73 añadirá el failover entre self._urls
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

    def commit(self, path: str, block_id: str, size: int, checksum_sha256: str) -> dict[str, Any]:
        return self._request("POST", "/files/commit", body={
            "path": path, "blockId": block_id, "size": size, "checksumSha256": checksum_sha256})

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
        try:
            resp = self._http.request(method, f"{self._base}{route}", params=params, json=body,
                                      headers=headers, timeout=self._timeout, verify=self._verify)
        except requests.RequestException as exc:
            raise NameNodeUnavailableError(f"No se pudo contactar al NameNode ({self._base})") from exc

        if resp.status_code >= 400:
            raise self._error_from(resp)
        if resp.status_code == 204 or not resp.content:
            return {}
        try:
            return resp.json()
        except ValueError:
            raise NameNodeError("Respuesta del NameNode no es JSON válido", resp.status_code) from None

    @staticmethod
    def _error_from(resp: requests.Response) -> NameNodeError:
        message, code = resp.reason or "Error", None
        try:
            payload = resp.json()
            message = payload.get("error", message)
            code = payload.get("code")
        except (ValueError, AttributeError):
            pass
        if resp.status_code == 503:
            return NameNodeStandbyError(message, resp.status_code, code)
        return NameNodeError(message, resp.status_code, code)