"""login: pide user/pass, obtiene el JWT y lo guarda en la sesión (US-81)."""
from __future__ import annotations

import getpass

from client.commands._common import namenode, remember_namenode
from client.core.errors import DfshaError
from client.core.namenode_api import pick


def run(args, config, session) -> int:
    username = args.username or input("Usuario: ").strip()
    if not username:
        raise DfshaError("Usuario vacío")
    password = getpass.getpass("Contraseña: ")

    api = namenode(config, session)
    resp = api.login(username, password)
    token = pick(resp, "token", "accessToken", "access_token")
    if not token:
        raise DfshaError("El NameNode no devolvió un token")

    session.token = str(token)
    session.username = username
    home = pick(resp, "home", "homeDir", "home_dir")
    session.cwd = str(home) if home else f"/user/{username}"
    session.save()
    remember_namenode(api, session)
    print(f"Sesión iniciada como {username} (cwd: {session.cwd})")
    return 0
