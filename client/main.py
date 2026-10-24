# Punto de entrada del CLI DFSha: parsea el comando (login, put, get, ls, cd, mkdir, rmdir, rm)
# y delega en client/commands/. Uso: python -m client.main <comando> [args]
"""CLI dfsha (US-30). Uso: python -m client.main --help"""
from __future__ import annotations

import argparse
import importlib
import sys

from client.config import load_config
from client.core.errors import DfshaError
from client.core.session import Session

# comando -> (módulo en client/commands, historia que lo implementa)
COMMANDS = {
    "login": ("login", "US-81"),
    "put": ("put", "US-34"),
    "get": ("get", "US-35"),
    "ls": ("namespace", "US-36"),
    "cd": ("namespace", "US-36"),
    "mkdir": ("namespace", "US-36"),
    "rmdir": ("namespace", "US-36"),
    "rm": ("namespace", "US-36"),
}


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="dfsha", description="Cliente del sistema de archivos distribuido DFSha")
    sub = parser.add_subparsers(dest="command", required=True, metavar="comando")

    p = sub.add_parser("login", help="iniciar sesión")
    p.add_argument("username", nargs="?", help="usuario (si se omite, se pregunta)")

    p = sub.add_parser("put", help="subir un archivo local al DFS")
    p.add_argument("local")
    p.add_argument("remote")

    p = sub.add_parser("get", help="descargar un archivo del DFS")
    p.add_argument("remote")
    p.add_argument("local")

    p = sub.add_parser("ls", help="listar un directorio")
    p.add_argument("path", nargs="?", default=".")

    p = sub.add_parser("cd", help="cambiar de directorio")
    p.add_argument("path")

    sub.add_parser("pwd", help="mostrar el directorio actual")

    p = sub.add_parser("mkdir", help="crear un directorio")
    p.add_argument("-p", "--parents", action="store_true", help="crear también los padres")
    p.add_argument("path")

    p = sub.add_parser("rmdir", help="borrar un directorio vacío")
    p.add_argument("path")

    p = sub.add_parser("rm", help="borrar un archivo")
    p.add_argument("path")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    config = load_config()
    session = Session.load(config.session_file)

    if args.command == "pwd":
        print(session.cwd)
        return 0

    module_name, story = COMMANDS[args.command]
    full_name = f"client.commands.{module_name}"
    try:
        module = importlib.import_module(full_name)
    except ModuleNotFoundError as exc:
        if exc.name == full_name:
            print(f"'{args.command}' aún no está implementado ({story}).", file=sys.stderr)
            return 2
        raise

    try:
        return module.run(args, config, session) or 0
    except DfshaError as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())