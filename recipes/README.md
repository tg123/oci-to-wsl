# Recipes

**Recipes** are ready-to-launch [`oci-to-wsl`](../README.md) YAML profiles. Each
recipe turns an OCI image into a purpose-built WSL distribution in a single
command — no manual `apt-get`, no copy-pasting setup scripts.

Because `oci-to-wsl --profile` accepts an `http(s)://` URL (like
`kubectl apply -f`), a recipe can be launched straight from this repository
without cloning it:

```powershell
# Some recipes stage helper scripts via files[].src. Opt in to fetching those
# relative sources from the same URL as the profile:
$env:OCI_TO_WSL_PROFILE_FOLLOW_URL = '1'

oci-to-wsl.exe --profile https://raw.githubusercontent.com/tg123/oci-to-wsl/main/recipes/kind/profile.yaml
```

`OCI_TO_WSL_PROFILE_FOLLOW_URL=1` lets a URL-loaded profile download its
relative `files[].src` from the **same** host and directory (cross-host
redirects and `../` escapes are refused). Recipes that ship a `*.sh` helper
need it; pure-`init_cmds` recipes do not.

## Anatomy of a recipe

Each recipe lives in its own directory under `recipes/<name>/` and contains:

| File | Purpose |
|---|---|
| `profile.yaml` | The `oci-to-wsl` profile (`image`, `wsl_conf`, `files`, `init_cmds`, …) |
| `README.md` | What it provisions, the launch one-liner, and any required host env vars |
| `*.sh` (optional) | Helper scripts staged into the rootfs via `files[].src` |

Conventions shared by every recipe:

- **systemd on** (`wsl_conf: [boot] systemd=true`) for anything that runs a
  long-lived service, so units start on first boot.
- **Idempotent** `init_cmds` / bootstrap scripts that tolerate re-running.
- **Pinned base image** (`image: repo:tag`) for reproducibility.
- **Same-origin helpers** — relative `files[].src` only, so the recipe works
  unchanged over a URL with `OCI_TO_WSL_PROFILE_FOLLOW_URL=1`.

## Available recipes

| Recipe | What you get |
|---|---|
| [`kind`](kind/) | A single-node Kubernetes control plane (`kindest/node`) running natively in WSL2, with a ready `kubeconfig` |

More recipes are welcome — copy an existing directory, adjust the profile, and
add a short README following the table above.
