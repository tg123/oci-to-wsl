# oci-to-wsl

Load an OCI container-registry image directly into a **Windows Subsystem for Linux** distribution – a single, self-contained binary with no runtime dependencies.

## Quick start

1. Download the latest `oci-to-wsl.exe` from the [GitHub Releases page](https://github.com/tg123/oci-to-wsl/releases/latest) and place it somewhere on your `PATH`.
2. Run it from PowerShell:

```powershell
# From Docker Hub (or, if already present, the local Docker daemon)
oci-to-wsl.exe --image ubuntu:22.04 --name my-ubuntu

# From Azure Container Registry (browser login opens automatically)
oci-to-wsl.exe --image myacr.azurecr.io/myimage:latest --name myimage

# Using a YAML profile
oci-to-wsl.exe --profile ubuntu.yaml
```

## Image sources

`oci-to-wsl` first looks up the requested image in the local Docker daemon
(discovered via `DOCKER_HOST` / the default socket). When the image is found
locally it is exported from the daemon directly, avoiding a registry round-trip.
Otherwise the image is pulled from its OCI registry as usual.

Set `OCI_TO_WSL_NO_LOCAL=1` (also accepts `true`/`yes`) to skip the local
lookup and always go to the registry:

```powershell
$env:OCI_TO_WSL_NO_LOCAL = '1'
oci-to-wsl.exe --image ubuntu:22.04 --name my-ubuntu
```

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
