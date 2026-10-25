"""ls, cd, mkdir, rmdir, rm (US-36). `cd` solo cambia el cwd local de la sesión."""
from __future__ import annotations

from client.commands._common import human_size, namenode, remember_namenode
from client.core.errors import DfshaError
from client.core.namenode_api import pick


def run(args, config, session) -> int:
    api = namenode(config, session)
    handler = {
        "ls": _ls, "cd": _cd, "mkdir": _mkdir, "rmdir": _rmdir, "rm": _rm,
    }[args.command]
    code = handler(args, api, session)
    remember_namenode(api, session)
    return code


# ---------- comandos ----------

def _ls(args, api, session) -> int:
    path = session.resolve(args.path)
    entries = _entries(api.ls(path))
    if not entries:
        return 0
    name_width = max(len(_name(e)) for e in entries)
    for e in sorted(entries, key=lambda e: (not _is_dir(e), _name(e))):
        kind = "d" if _is_dir(e) else "-"
        size = "" if _is_dir(e) else human_size(pick(e, "size", default=0))
        modified = pick(e, "modified", "modifiedAt", "modified_at", "mtime", default="")
        print(f"{kind} {_name(e):<{name_width}}  {size:>10}  {modified}")
    return 0


def _cd(args, api, session) -> int:
    target = session.resolve(args.path)
    info = api.ls(target)  # 404 si no existe
    kind = pick(info, "type", "kind")
    if kind and str(kind).lower() not in ("dir", "directory"):
        raise DfshaError(f"{target} no es un directorio")
    session.cwd = target
    session.save()
    return 0


def _mkdir(args, api, session) -> int:
    api.mkdir(session.resolve(args.path), parents=bool(getattr(args, "parents", False)))
    return 0


def _rmdir(args, api, session) -> int:
    api.rmdir(session.resolve(args.path))
    return 0


def _rm(args, api, session) -> int:
    api.rm(session.resolve(args.path))
    return 0


# ---------- lectura tolerante de la respuesta de ls ----------

def _entries(payload) -> list[dict]:
    if isinstance(payload, list):
        return payload
    if isinstance(payload, dict):
        for key in ("entries", "items", "children", "files"):
            if isinstance(payload.get(key), list):
                return payload[key]
    return []


def _name(e: dict) -> str:
    return str(pick(e, "name", "path", default="?"))


def _is_dir(e: dict) -> bool:
    kind = pick(e, "type", "kind")
    if kind is not None:
        return str(kind).lower() in ("dir", "directory")
    return bool(pick(e, "isDir", "is_dir", "directory", default=False))
