"""Excepciones propias del cliente.

Los errores de DataNode llevan el id del nodo, nunca host/IP
(el usuario no debe ver direcciones de DataNodes).
"""
from __future__ import annotations


class DfshaError(Exception):
    """Base de todos los errores del cliente."""


class NameNodeError(DfshaError):
    def __init__(self, message: str, status: int | None = None, code: str | None = None):
        super().__init__(message)
        self.status = status
        self.code = code


class NameNodeStandbyError(NameNodeError):
    """El NameNode respondió 503: es un standby."""


class NameNodeUnavailableError(DfshaError):
    """No se pudo contactar al NameNode (timeout o conexión)."""


class DataNodeError(DfshaError):
    def __init__(self, message: str, node_id: str | None = None, block_id: str | None = None):
        super().__init__(message)
        self.node_id = node_id
        self.block_id = block_id


class BlockNotFoundError(DataNodeError):
    pass


class BlockCorruptedError(DataNodeError):
    """Checksum en disco o en tránsito no coincide: probar otra réplica."""


class DataNodeUnavailableError(DataNodeError):
    pass