"""Configuración del cliente por variables de entorno (US-30)."""
from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

DEFAULT_NAMENODES = "http://localhost:8080"


def _env_float(name: str, default: float) -> float:
    raw = os.environ.get(name)
    if raw is None or raw.strip() == "":
        return default
    try:
        return float(raw)
    except ValueError:
        raise SystemExit(f"Variable {name} inválida: {raw!r} (se esperaba un número)")


@dataclass(frozen=True)
class Config:
    namenodes: tuple[str, ...]
    block_size_mb: float
    http_timeout: float
    grpc_timeout: float
    max_threads: int
    ca_cert: str | None
    session_file: Path

    @property
    def block_size_bytes(self) -> int:
        return max(1, int(self.block_size_mb * 1024 * 1024))


def load_config() -> Config:
    raw_urls = os.environ.get("DFSHA_NAMENODES", DEFAULT_NAMENODES)
    urls = tuple(u.strip().rstrip("/") for u in raw_urls.split(",") if u.strip())
    if not urls:
        raise SystemExit("DFSHA_NAMENODES está vacío")

    session_file = os.environ.get("DFSHA_SESSION_FILE")
    return Config(
        namenodes=urls,
        block_size_mb=_env_float("DFSHA_BLOCK_SIZE_MB", 8),
        http_timeout=_env_float("DFSHA_HTTP_TIMEOUT", 10),
        grpc_timeout=_env_float("DFSHA_GRPC_TIMEOUT", 60),
        max_threads=max(1, int(_env_float("DFSHA_MAX_THREADS", 4))),
        ca_cert=os.environ.get("DFSHA_CA_CERT") or None,
        session_file=Path(session_file) if session_file else Path.home() / ".dfsha" / "session.json",
    )