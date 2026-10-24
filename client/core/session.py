"""Sesión del usuario: token, directorio actual y NameNode activo (US-30)."""
from __future__ import annotations

import json
import os
import posixpath
from dataclasses import asdict, dataclass, field, fields
from pathlib import Path


@dataclass
class Session:
    token: str | None = None
    username: str | None = None
    cwd: str = "/"
    active_namenode: str | None = None  # lo usará US-73
    path: Path = field(default_factory=lambda: Path.home() / ".dfsha" / "session.json",
                       repr=False, compare=False)

    @classmethod
    def load(cls, path: Path) -> "Session":
        session = cls(path=path)
        try:
            data = json.loads(Path(path).read_text(encoding="utf-8"))
        except (FileNotFoundError, json.JSONDecodeError, OSError):
            return session  # sin sesión o archivo corrupto: sesión limpia
        known = {f.name for f in fields(cls)} - {"path"}
        for key, value in data.items():
            if key in known:
                setattr(session, key, value)
        if not session.cwd:
            session.cwd = "/"
        return session

    def save(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        data = {k: v for k, v in asdict(self).items() if k != "path"}
        tmp = self.path.with_suffix(".tmp")
        tmp.write_text(json.dumps(data, indent=2), encoding="utf-8")
        try:
            os.chmod(tmp, 0o600)  # contiene el JWT; en Windows no tiene efecto
        except OSError:
            pass
        os.replace(tmp, self.path)

    def resolve(self, path: str) -> str:
        """Convierte una ruta (relativa o absoluta) en absoluta normalizada."""
        joined = path if path.startswith("/") else posixpath.join(self.cwd, path)
        normalized = posixpath.normpath(joined)
        if normalized.startswith("//"):  # normpath conserva '//' inicial
            normalized = "/" + normalized.lstrip("/")
        return normalized