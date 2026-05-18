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

Set `OCI_TO_WSL_NO_LOCAL=1` (also accepts `true`/`True`/`TRUE`/`t`) to skip the
local lookup and always go to the registry:

```powershell
$env:OCI_TO_WSL_NO_LOCAL = '1'
oci-to-wsl.exe --image ubuntu:22.04 --name my-ubuntu
```

## Cross-platform tars (save-tar mode)

When importing into WSL the image platform is always the host's: importing an
arm rootfs into an x86 WSL (or vice versa) does not work, so there is no CLI
flag for it.

In `--save-tar` mode you can override the platform via the
`OCI_TO_WSL_PLATFORM` environment variable (format `os/arch`, e.g.
`linux/arm64`, `windows/amd64`). The variable is ignored outside save-tar
mode.

```powershell
$env:OCI_TO_WSL_PLATFORM = 'linux/arm64'
oci-to-wsl.exe --image ubuntu:22.04 --save-tar ubuntu-arm64.tar
```

Profile-driven tar modifications (`copies` and `deletes`) are applied to the
rootfs tar in `--save-tar` mode as well, so the saved artifact matches what
would be imported into WSL. Set `OCI_TO_WSL_NO_TAR_MODS=1` (also accepts
`true`/`True`/`TRUE`/`t`) to skip them and obtain the rootfs exactly as
exported from the image:

```powershell
$env:OCI_TO_WSL_NO_TAR_MODS = '1'
oci-to-wsl.exe --profile ubuntu.yaml --save-tar ubuntu.tar
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
copies:                          # optional – injected into the rootfs tar so files exist on first boot
  - src: ./scripts/bootstrap.sh  # relative paths resolve to the profile's directory
    dst: /usr/local/bin/bootstrap.sh
    mode: "0755"                 # optional – octal, e.g. "0755" or "777"
  - src: C:\Users\me\assets      # native Windows paths are accepted
    dst: /opt/assets
  - src: '%USERPROFILE%\.gitconfig'  # %VAR%, $VAR / ${VAR} and a leading ~ are expanded
    dst: /root/.gitconfig
deletes:                         # optional – absolute POSIX paths dropped from the rootfs tar before import
  - /var/cache/apt               # directories are removed recursively; applied before `copies`
  - /etc/motd
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
| `--save-tar <path>` | Write the rootfs tar to `<path>` and skip `wsl --import` |
| `--loglevel <level>` | Logging verbosity: `debug`, `info` (default), `warn`, or `error` |
