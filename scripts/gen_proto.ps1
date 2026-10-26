# Genera el código desde proto/: stubs Go en services/gen/ y stubs Python en client/gen/ (US-04).
#
# Requisitos:
#   - Python con grpcio-tools (trae su propio protoc):  pip install -r client/requirements.txt
#   - Plugins Go en $env:USERPROFILE\go\bin:
#       go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
#       go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
#
# Uso (desde cualquier carpeta):  .\scripts\gen_proto.ps1
# Si cambias un .proto, corre este script y commitea lo generado en el mismo PR.

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# Python del venv si existe; si no, el del PATH.
$python = Join-Path $root ".venv\Scripts\python.exe"
if (-not (Test-Path $python)) { $python = "python" }

# Asegura que protoc encuentre los plugins de Go.
$goBin = Join-Path $env:USERPROFILE "go\bin"
if (Test-Path $goBin) { $env:PATH = "$goBin;$env:PATH" }

$protos = @("proto/datanode.proto", "proto/namenode.proto")
$goOut = "services/gen"
$pyOut = "client/gen"

New-Item -ItemType Directory -Force -Path $goOut, $pyOut | Out-Null

Write-Host "==> Go  -> $goOut"
& $python -m grpc_tools.protoc -I proto `
    --go_out=$goOut --go_opt=module=dfsha/services/gen `
    --go-grpc_out=$goOut --go-grpc_opt=module=dfsha/services/gen `
    @protos
if ($LASTEXITCODE -ne 0) { throw "Fallo generando Go" }

Write-Host "==> Python -> $pyOut"
& $python -m grpc_tools.protoc -I proto `
    --python_out=$pyOut --pyi_out=$pyOut --grpc_python_out=$pyOut `
    @protos
if ($LASTEXITCODE -ne 0) { throw "Fallo generando Python" }

Write-Host "OK"
