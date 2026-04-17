# remote-tools

A single static binary for short-lived DevOps and testing work on remote
VPSes. Download it, run it, do your work, stop it, delete it — no root, no
system writes outside `/tmp`, no leftover Tailscale node in your admin list.

## Why

When you just need to poke at a VPS for a few minutes — tail a log, grab a
crash dump, reach a local-only service — installing `tailscaled` or editing
SSH/nginx configs is overkill and leaves traces. `remote-tools` embeds a
userspace Tailscale node directly in the binary via
[`tsnet`](https://tailscale.com/kb/1244/tsnet), so you get a tailnet
identity, a read-only file server, and TCP port forwarding from a single
binary that needs no admin rights.

On `Ctrl-C` the process logs the node out of the tailnet so it disappears
from your admin list immediately, then wipes its `/tmp` state dir. If the
process is killed ungracefully, the node is still marked ephemeral and gets
garbage-collected by the control plane.

## Installation

Download the latest release binary and make it executable:

```sh
curl -L https://github.com/avbel/remote-tools/releases/latest/download/remote-tools-linux-amd64 \
  -o remote-tools
chmod +x remote-tools
```

That's the whole install. There is nothing to configure, no package to
install, no daemon to register.

## Quick start

Generate an **ephemeral** auth key in the Tailscale admin console
(Settings → Keys → Generate auth key → Ephemeral), then on the VPS:

```sh
# Read-only file browser at http://remote-tools-<vps>:8080/
./remote-tools --ts-authkey=tskey-... --serve-dir=/var/log

# Plus forward the local Postgres onto the tailnet:
./remote-tools \
  --ts-authkey=tskey-... \
  --serve-dir=/var/log \
  --expose 5432=localhost:5432

# Tailnet only — no file server, no forwards — for ad-hoc use.
./remote-tools --ts-authkey=tskey-...
```

From any other device on your tailnet:

```sh
# Browse
open http://remote-tools-<vps>:8080/

# Resumable download (curl -C -)
curl -C - -O "http://remote-tools-<vps>:8080/syslog?download=1"

# One-shot directory archive
curl -o logs.tgz "http://remote-tools-<vps>:8080/?archive=tgz"

# Reach the forwarded Postgres
psql -h remote-tools-<vps> -p 5432 -U postgres
```

Stop it with `Ctrl-C`. The node is removed from the Tailscale admin list
right away, `/tmp/remote-tools-*` is wiped, and you can `rm remote-tools` to
leave no trace.

## Flags

| Flag | Env var | Default | Description |
|---|---|---|---|
| `--ts-authkey` | `TS_AUTHKEY`, `REMOTE_TOOLS_TS_AUTHKEY` | *(required)* | Tailscale ephemeral auth key. |
| `--ts-hostname` | `REMOTE_TOOLS_TS_HOSTNAME` | `remote-tools-<host>` | Hostname on the tailnet. |
| `--serve-dir` | `REMOTE_TOOLS_SERVE_DIR` | *(disabled)* | Directory to expose read-only. |
| `--serve-port` | `REMOTE_TOOLS_SERVE_PORT` | `8080` | Tailnet port for the file server. |
| `--expose` | — | *(none)* | `PORT=HOST:PORT` forward. Repeatable. |
| `--verbose` | — | `false` | Verbose tsnet logging. |
| `--version` | — | — | Print version and exit. |

## File server features

- Directory browsing with sortable listing.
- `?download=1` forces `Content-Disposition: attachment`.
- `?archive=tgz` on a directory streams a gzip tarball.
- Range requests (resumable downloads with `curl -C -`).
- Read-only: only `GET` and `HEAD` are accepted.
- Served **only** on the tailnet listener — nothing is bound to public
  interfaces on the VPS.
- Concurrent by default: each accepted connection is handled in its own
  goroutine, so multiple parallel downloads (e.g. from a download manager
  with segmented downloads, or several clients at once) don't block each
  other.

## Zero-trace guarantees

- All Tailscale state lives in a `/tmp/remote-tools-*` directory created at
  startup with `os.MkdirTemp` and removed on exit.
- Logs go to stderr only — no log files, config files, or caches.
- On `SIGINT`/`SIGTERM` the node calls `Logout` before closing, so it
  disappears from your tailnet admin list immediately.
- `Ephemeral: true` means that if the process is SIGKILL'd, the control
  plane still removes the node once it's been offline past the ephemeral
  timeout.

## Building from source

```sh
CGO_ENABLED=0 go build \
  -trimpath \
  -tags "ts_omit_aws ts_omit_bird ts_omit_tap ts_omit_kube ts_omit_capture" \
  -ldflags "-s -w" \
  -o remote-tools .
```

The build tags strip unused tsnet subsystems to keep the binary small.

## Security notes

- The file server is **read-only**, but there is no additional access
  control layered on top; treat your tailnet ACL as the authz boundary.
- Symlinks under `--serve-dir` are followed for regular file reads (same
  semantics as `http.FileServer`), but the tar.gz archive stream skips them
  to avoid escaping the root.
- Use an **ephemeral** auth key. Reusable or persistent keys defeat the
  "no traces" promise.
