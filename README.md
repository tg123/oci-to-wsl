# oci-to-wsl

Load an OCI container-registry image directly into a **Windows Subsystem for Linux** distribution – a single, self-contained binary with no runtime dependencies.

## Quick start

1. Download the latest `oci-to-wsl.exe` from the [GitHub Releases page](https://github.com/tg123/oci-to-wsl/releases/latest) and place it somewhere on your `PATH`.
2. Run it from PowerShell:

```powershell
# From Docker Hub
oci-to-wsl.exe --image ubuntu:22.04 --name my-ubuntu

# From Azure Container Registry (browser login opens automatically)
oci-to-wsl.exe --image myacr.azurecr.io/myimage:latest --name myimage

# Using a YAML profile
oci-to-wsl.exe --profile ubuntu.yaml
```

## Logging in to a private registry (no docker CLI required)

`oci-to-wsl` reads classic basic-auth entries from `~/.docker/config.json`
(the same file `docker login` writes), so any credentials already saved by
the docker CLI are picked up automatically. If you don't have docker
installed, the bundled `dockerlogin` subcommand can write the same file
itself:

```powershell
# Interactive login to Docker Hub
oci-to-wsl.exe dockerlogin

# GHCR with an explicit token, no prompting
oci-to-wsl.exe dockerlogin ghcr.io --username alice --password-stdin < token.txt

# Custom config path
oci-to-wsl.exe dockerlogin myregistry.example.com -u alice -p s3cret --config C:\creds\config.json

# Write the resulting config.json to a different file (or '-' for stdout)
oci-to-wsl.exe dockerlogin ghcr.io -u alice -p s3cret -o C:\creds\out.json
oci-to-wsl.exe dockerlogin ghcr.io -u alice -p s3cret -o -
```

Only the classic base64-encoded `username:password` format is written;
credential helpers / stores are not invoked. The on-disk format is produced
by docker's own `github.com/docker/cli/cli/config` package, so the resulting
file is byte-for-byte identical to what `docker login` writes for the same
inputs (verified by a unit test).

## YAML profile

```yaml
# ubuntu.yaml
name: my-ubuntu
image: ubuntu:22.04
install_dir: C:\WSL\my-ubuntu   # optional – defaults to .\<name>
init_cmds:                       # optional – run inside the new distro after import
  - apt-get update -y
  - apt-get install -y curl git
```

See [`example-profile.yaml`](example-profile.yaml) for a complete example.

## Building from source

```powershell
# Requires Go 1.21+
$env:GOOS = 'windows'; $env:GOARCH = 'amd64'; go build -o oci-to-wsl.exe .
```

## How ACR authentication works

ACR auth is delegated to the official [Azure SDK for Go](https://github.com/Azure/azure-sdk-for-go):

1. **`azidentity.AzureCLICredential`** – if you have already run `az login`, the cached token is reused with no prompting.
2. **`azidentity.InteractiveBrowserCredential`** – otherwise, the default browser is opened for sign-in (no device-code copy/paste required).
3. The resulting AAD token is exchanged for a scoped ACR access token via `azcontainerregistry.AuthenticationClient`.

No credentials are stored on disk by this tool.

## CLI flags

| Flag | Description |
|---|---|
| `--profile <path>` | Path to a YAML profile file |
| `--image <ref>` | OCI image reference (required without `--profile`) |
| `--name <distro>` | WSL distribution name (required without `--profile`) |
| `--dir <path>` | Directory to store the WSL virtual disk (default: `.\<name>`) |
