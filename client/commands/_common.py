"""Helpers compartidos por los comandos."""
from __future__ import annotations

from client.config import Config
from client.core.datanode_client import DataNodeClient
from client.core.namenode_api import NameNodeAPI
from client.core.session import Session


def namenode(config: Config, session: Session) -> NameNodeAPI:
    return NameNodeAPI(config.namenodes, token=session.token, timeout=config.http_timeout,
                       ca_cert=config.ca_cert, preferred=session.active_namenode)


def datanode(config: Config) -> DataNodeClient:
    return DataNodeClient(timeout=config.grpc_timeout, ca_cert=config.ca_cert)


def remember_namenode(api: NameNodeAPI, session: Session) -> None:
    """Guarda en la sesión el NameNode que respondió (para el próximo comando)."""
    if session.active_namenode != api.active_url:
        session.active_namenode = api.active_url
        session.save()


def human_size(n: int | float | None) -> str:
    if n is None:
        return "-"
    n = float(n)
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024 or unit == "TB":
            return f"{n:.0f} {unit}" if unit == "B" else f"{n:.1f} {unit}"
        n /= 1024
    return f"{n:.1f} TB"
