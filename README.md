# oci-to-wsl

Load an OCI container-registry image directly into a **Windows Subsystem for Linux** distribution – a single, self-contained binary with no runtime dependencies.

## Quick start

1. Install `oci-to-wsl`:

   ```powershell
   winget install tg123.oci-to-wsl
   ```

   Alternatively, download the latest `oci-to-wsl.exe` from the [GitHub Releases page](https://github.com/tg123/oci-to-wsl/releases/latest) and place it somewhere on your `PATH`, or run the one-liner below from PowerShell to fetch and extract it into the current directory:

   ```powershell
   $arch=switch(($env:PROCESSOR_ARCHITECTURE+'').ToUpperInvariant()){'ARM64'{'arm64'}'AMD64'{'x86_64'}default{throw "Unsupported architecture '$env:PROCESSOR_ARCHITECTURE'. Supported values: AMD64, ARM64."}}; $url="https://github.com/tg123/oci-to-wsl/releases/latest/download/oci-to-wsl_windows_$arch.zip"; $zip=Join-Path $env:TEMP "oci-to-wsl.zip"; $tmp=Join-Path $env:TEMP ("oci-to-wsl-"+[guid]::NewGuid()); New-Item -ItemType Directory -Path $tmp | Out-Null; Invoke-WebRequest $url -OutFile $zip; Expand-Archive -Force $zip -DestinationPath $tmp; Move-Item -Force (Join-Path $tmp "oci-to-wsl.exe") .; Remove-Item -Recurse -Force $tmp; Remove-Item $zip
   ```


2. Run it from PowerShell:

```powershell
# From Docker Hub (or, if already present, the local Docker daemon)
oci-to-wsl.exe --image ubuntu:22.04 --name my-ubuntu

# From Azure Container Registry (browser login opens automatically)
oci-to-wsl.exe --image myacr.azurecr.io/myimage:latest --name myimage

# Using a YAML profile
oci-to-wsl.exe --profile ubuntu.yaml

# Read a profile from stdin (like `kubectl apply -f -`)
cat ubuntu.yaml | oci-to-wsl.exe --profile -

# Fetch a profile over the network
oci-to-wsl.exe --profile https://example.com/ubuntu.yaml

# Opt in to also downloading the profile's relative `files` sources from the
# same URL (off by default; see the YAML profile section below)
$env:OCI_TO_WSL_PROFILE_FOLLOW_URL=1; oci-to-wsl.exe --profile https://example.com/ubuntu.yaml
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

## Azure Container Registry authentication

For `*.azurecr.io`, `*.azurecr.cn`, and `*.azurecr.us` hosts `oci-to-wsl`
automatically acquires an AAD token (reusing your `az login` session, then
falling back to interactive browser sign-in). Set `OCI_TO_WSL_NO_ACR_AAD=1`
(also accepts `true`/`True`/`TRUE`/`t`) to disable AAD entirely and use the
normal docker keychain instead, so a username/password (or token) from
`docker login` is honored just like for any other registry:

```powershell
$env:OCI_TO_WSL_NO_ACR_AAD = '1'
docker login myacr.azurecr.io      # or set $env:DOCKER_CONFIG
oci-to-wsl.exe --image myacr.azurecr.io/myimage:latest --name myimage
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
name: '%USERNAME%-ubuntu'           # %VAR% / $VAR / ${VAR} expanded from host env
image: ubuntu:22.04                 # also accepts %VAR% / $VAR / ${VAR} (e.g. '%ACR_REGISTRY%/ubuntu:22.04')
install_dir: '%USERPROFILE%\WSL\my-ubuntu'   # optional – defaults to .\<name>; %VAR% / $VAR / ${VAR} and leading ~ expanded
files:                           # optional – injected into the rootfs tar so files exist on first boot
  - src: ./scripts/bootstrap.sh  # relative paths resolve to the profile's directory
    dst: /usr/local/bin/bootstrap.sh
    mode: "0755"                 # optional – octal, e.g. "0755" or "777"
    sha1: da39a3ee5e6b4b0d3255bfef95601890afd80709  # optional – 40-char hex digest the staged bytes must match (any src)
  - src: C:\Users\me\assets      # native Windows paths are accepted
    dst: /opt/assets
    replace: false               # optional – default true; false overlays onto the upstream tree instead of replacing it
  - src: https://example.com/tools/installer.sh  # http:// / https:// URLs are downloaded at staging time
    dst: /usr/local/bin/installer.sh
    mode: "0755"
    sha1: da39a3ee5e6b4b0d3255bfef95601890afd80709  # optional – 40-char hex digest the staged bytes must match (any src)
  - src: '%USERPROFILE%\.gitconfig'  # %VAR%, $VAR / ${VAR} and a leading ~ are expanded in `src`
    dst: '/home/%USERNAME%/.gitconfig' # `dst` accepts the same %VAR% / $VAR / ${VAR} expansion as `src`
  - dst: /etc/motd               # inline UTF-8 body in place of 'src'
    content: |
      Welcome to my-ubuntu
  - dst: /opt/secret.bin         # inline binary body, base64-encoded
    content_base64: aGVsbG8gd29ybGQK
    mode: "0600"
deletes:                         # optional – absolute POSIX paths dropped from the rootfs tar before import
  - /var/cache/apt               # directories are removed recursively; applied before `files`
  - /etc/motd
  - '/home/%USERNAME%/.cache'    # %VAR% / $VAR / ${VAR} are expanded the same as `files.dst`
users:                           # optional – Linux users created by editing /etc/passwd, /etc/shadow, /etc/group
  - name: "%USERNAME%"           # required; %VAR% / $VAR / ${VAR} expanded from host env (e.g. mirror Windows login into WSL)
    uid: 1000                    # optional; auto-allocated from 1000+ when omitted
    gid: 1000                    # optional; defaults to uid (matching primary group created on demand)
    home: /home/alice            # optional; defaults to /home/<name>
    shell: /bin/bash             # optional; defaults to /bin/sh
    gecos: "Alice Example"       # optional comment / full name
    groups: [sudo, wheel]        # optional; supplementary groups (missing groups silently skipped)
    password_hash: "$6$..."      # optional; written verbatim into /etc/shadow (e.g. `openssl passwd -6`)
    password_plain: "s3cret!"    # optional; hashed with SHA-512 crypt before write (mutually exclusive with password_hash)
    no_create_home: false        # optional; when true, suppresses the home directory tar entry
wsl_conf:                        # optional – syntactic sugar for writing /etc/wsl.conf
  mode: merge                    # "merge" (default) merges with any existing /etc/wsl.conf; "replace" overwrites
  content: |                     # raw INI string – %VAR%, $VAR and ${VAR} are expanded against the host environment
    [boot]
    systemd=true
    [user]
    default=%USERNAME%
  # `content` also accepts a YAML mapping of sections, which is rendered to
  # the same INI body (env-var expansion still applies):
  # content:
  #   boot:
  #     systemd: true
  #   user:
  #     default: "%USERNAME%"
init_cmds:                       # optional – run inside the new distro after import
  - apt-get update -y
  - apt-get install -y curl git
```

See [`example-profile.yaml`](example-profile.yaml) for a complete example.

When the profile is loaded from a `-`/stdin or an `http(s)://` URL there is no
enclosing directory, so relative `files[].src` paths resolve against the
current working directory. For URL profiles you can instead opt in to
downloading those relative sources from the **same URL** by setting
`OCI_TO_WSL_PROFILE_FOLLOW_URL=1` (also accepts `true`/`True`/`TRUE`/`t`):

```powershell
$env:OCI_TO_WSL_PROFILE_FOLLOW_URL = '1'
oci-to-wsl.exe --profile https://example.com/profiles/ubuntu.yaml
```

This is **off by default** because it widens the trust placed in the remote
profile. When enabled, fetched file references are constrained for safety:
each `src` must be a relative path that stays on the **same scheme and host**
as the profile URL and **within the profile's directory** (no absolute URLs,
no `../` escapes), and cross-host redirects are refused. Downloaded files are size-limited like the profile itself (default 1 MiB;
override via `OCI_TO_WSL_MAX_PROFILE_SIZE` in bytes).

## GitHub Action

`oci-to-wsl` ships as a reusable GitHub Action so you can import an OCI image
into a WSL distribution from a workflow. The action downloads the matching
released `oci-to-wsl.exe` and runs it for you.

> [!IMPORTANT]
> **Runner requirement:** the action only works on Windows runners that have
> **WSL 2** enabled, because importing a rootfs uses `wsl --import` (a WSL 2
> feature). Use `windows-2025` — on the GitHub-hosted images WSL 2 is enabled
> by default only on Windows Server 2025; `windows-2022` ships WSL 1 only.
> `windows-latest` is acceptable once it points to Windows Server 2025; until
> then pin `runs-on: windows-2025`. Self-hosted Windows runners must have the
> WSL 2 feature installed.

Import a simple image:

```yaml
jobs:
  wsl:
    runs-on: windows-2025
    steps:
      - uses: tg123/oci-to-wsl@main
        with:
          image: ubuntu:22.04
          name: my-ubuntu

      - run: wsl -d my-ubuntu -- cat /etc/os-release
```

Use a YAML profile (which can stage `files`, create `users`, run `init_cmds`,
etc.) instead of a bare image:

```yaml
jobs:
  wsl:
    runs-on: windows-2025
    steps:
      - uses: actions/checkout@v6
      - uses: tg123/oci-to-wsl@main
        with:
          profile: ./ubuntu.yaml
```

### Action inputs

| Input | Description |
|---|---|
| `image` | OCI image reference to import (ignored when `profile` is set) |
| `name` | WSL distribution name (required unless the profile sets it or `save-tar` is used) |
| `profile` | Path to a YAML profile file (or `-` for stdin / `http(s)://` URL); overrides `image`/`name`/`dir` when set |
| `dir` | Directory to store the WSL virtual disk (default: `.\<name>`) |
| `save-tar` | Write the rootfs tar to this path and skip `wsl --import` |
| `loglevel` | Logging verbosity: `debug`, `info` (default), `warn`, or `error` |
| `version` | Release of `oci-to-wsl` to download, e.g. `v1.2.3` (default: `latest`) |

### Action outputs

| Output | Description |
|---|---|
| `binary` | Path to the `oci-to-wsl.exe` binary that was downloaded and used |

See [`.github/workflows/example-action.yml`](.github/workflows/example-action.yml)
for a complete example.

### Testing the action

The action is smoke-tested in CI by
[`.github/workflows/action-test.yml`](.github/workflows/action-test.yml): it
runs the action from the local checkout (`uses: ./`) on a `windows-2025`
runner, imports `alpine:3.20`, and asserts a command runs inside the resulting
distribution. To verify a change to `action.yml`, run that workflow (it also
triggers on `workflow_dispatch`) or copy the job into your own repository and
point `uses:` at the branch/tag you want to test.

### Publishing to the GitHub Marketplace

This repository already contains the metadata the Marketplace requires: an
[`action.yml`](action.yml) in the repository root with a unique `name`, a
`description`, and a `branding` (icon + color) block. To publish it:

1. Make sure the repository is **public** and that `action.yml` lives in the
   repository root (a repo can only publish one Marketplace action from its
   root).
2. Draft a new release: **Releases → Draft a new release** and choose a
   semantic-version tag (e.g. `v1`).
3. GitHub detects `action.yml` and shows a **"Publish this Action to the
   GitHub Marketplace"** checkbox above the release notes — tick it, accept
   the GitHub Marketplace Developer Agreement (first time only), and pick a
   primary and secondary category.
4. Resolve anything flagged in the validation checklist (a unique action name,
   a complete `branding` block, etc.), then **Publish release**.

After it is live, consumers reference it as `tg123/oci-to-wsl@v1`. Moving the
`v1` tag to each new release lets them stay on the major version; the action's
`version` input still controls which `oci-to-wsl.exe` release is downloaded at
run time.

## Building from source

```powershell
# Requires Go 1.21+
$env:GOOS = 'windows'; $env:GOARCH = 'amd64'; go build -o oci-to-wsl.exe .
```

## How ACR authentication works

ACR auth is delegated to the official [Azure SDK for Go](https://github.com/Azure/azure-sdk-for-go) and applies to `*.azurecr.io`, `*.azurecr.cn`, and `*.azurecr.us` hosts:

1. **`azidentity.AzureCLICredential`** – if you have already run `az login`, the cached token is reused with no prompting. CLI failures such as "not logged in" are treated as a soft failure and fall through to the browser flow.
2. **`azidentity.InteractiveBrowserCredential`** – otherwise, the default browser is opened for sign-in (no device-code copy/paste required).
3. The resulting AAD token is exchanged for a scoped ACR access token via `azcontainerregistry.AuthenticationClient`.

Set `OCI_TO_WSL_NO_ACR_AAD=1` to bypass this entirely and use the docker keychain (`~/.docker/config.json` / `docker login`) instead — see the [Azure Container Registry authentication](#azure-container-registry-authentication) section above.

No credentials are stored on disk by this tool.

## CLI flags

| Flag | Description |
|---|---|
| `--profile <path>` | Path to a YAML profile file. Use `-` to read from stdin or an `http(s)://` URL to fetch one over the network (like `kubectl apply -f`) |
| `--image <ref>` | OCI image reference (required without `--profile`) |
| `--name <distro>` | WSL distribution name (required without `--profile`) |
| `--dir <path>` | Directory to store the WSL virtual disk (default: `.\<name>`) |
| `--save-tar <path>` | Write the rootfs tar to `<path>` and skip `wsl --import` |
| `--loglevel <level>` | Logging verbosity: `debug`, `info` (default), `warn`, or `error` |
