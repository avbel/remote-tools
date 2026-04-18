# remote-tools
<p align="center">
<img src="assets/logo.png" width="300" alt="remote-tools logo">
</p>

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

Every release ships three linux/amd64 variants. Pick whichever fits your
environment; they behave identically at runtime.

| Artifact | Size | When to use |
|---|---|---|
| `remote-tools-linux-amd64` | ~27 MB | Default. No decompression step. Works everywhere. |
| `remote-tools-linux-amd64.gz` | ~10 MB | Smaller download. Adds one `gunzip` step. Zero runtime cost. |
| `remote-tools-linux-amd64.upx` | ~7.4 MB | Smallest. Startup ~30 ms slower due to in-process decompression. May trigger AV false positives on Windows or corporate firewalls. |

One-liner (plain binary):

```sh
curl -fsSL -o remote-tools \
  https://github.com/avbel/remote-tools/releases/latest/download/remote-tools-linux-amd64 \
  && chmod +x remote-tools
```

One-liner (gzip — smallest transfer, no runtime surprises):

```sh
curl -fsSL https://github.com/avbel/remote-tools/releases/latest/download/remote-tools-linux-amd64.gz \
  | gunzip > remote-tools && chmod +x remote-tools
```

One-liner (UPX):

```sh
curl -fsSL -o remote-tools \
  https://github.com/avbel/remote-tools/releases/latest/download/remote-tools-linux-amd64.upx \
  && chmod +x remote-tools
```

Pin a specific version (recommended for reproducible runs):

```sh
VERSION=v0.1.0
curl -fsSL -o remote-tools \
  "https://github.com/avbel/remote-tools/releases/download/${VERSION}/remote-tools-linux-amd64" \
  && chmod +x remote-tools
```

Verify the checksum (one `SHA256SUMS` file covers all three variants):

```sh
curl -fsSL -o SHA256SUMS \
  https://github.com/avbel/remote-tools/releases/latest/download/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
```

Run straight from `/tmp` if you want to keep your working directory clean
(nothing else touches disk outside `/tmp` anyway):

```sh
curl -fsSL https://github.com/avbel/remote-tools/releases/latest/download/remote-tools-linux-amd64.gz \
  | gunzip > /tmp/remote-tools \
  && chmod +x /tmp/remote-tools \
  && /tmp/remote-tools --ts-authkey=tskey-... --serve-dir=/var/log
```

That's the whole install. There is nothing to configure, no package to
install, no daemon to register. `rm /tmp/remote-tools` when you're done.

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

## Running detached via `systemd-run`

If you don't want to hold a terminal open (or you're on a flaky SSH
connection), `systemd-run --user` starts `remote-tools` as a transient
user-level service — still **no root, no unit files on disk** — and stops
it cleanly on demand, which in turn triggers the tailnet logout.

```sh
# Start as a transient user service.
systemd-run --user \
  --unit=remote-tools \
  --collect \
  --property=Environment=TS_AUTHKEY=tskey-... \
  $(pwd)/remote-tools --serve-dir=/var/log

# Follow logs.
journalctl --user -u remote-tools -f

# Stop it (sends SIGTERM -> graceful Logout -> node removed from admin list).
systemctl --user stop remote-tools
```

Notes:

- `--collect` makes systemd forget the unit as soon as it exits, so nothing
  lingers in the user manager's state.
- Passing the auth key via `Environment=` keeps it out of `ps`/command-line
  history; `--ts-authkey=` works too if you prefer.
- `loginctl enable-linger <user>` once, if you want the service to survive
  logout on a host where your user has no persistent session.
- Running without `--user` (i.e. system-wide) would require root and
  defeats the zero-privilege story — stick to `--user`.

If systemd-run is unavailable, the portable alternative is:

```sh
TS_AUTHKEY=tskey-... nohup ./remote-tools --serve-dir=/var/log >/tmp/remote-tools.log 2>&1 &
# later
kill -TERM %1   # or: kill -TERM <pid>
```

`SIGTERM` triggers the same graceful Logout as `Ctrl-C`.

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

- Directory browsing with sortable listing at `/`.
- `?download=1` forces `Content-Disposition: attachment`.
- `?archive=tgz` on a directory streams a gzip tarball.
- Range requests (resumable downloads with `curl -C -`).
- Read-only **WebDAV** at `/dav/` for mounting in a file manager (see below).
- Only `GET`/`HEAD`/`OPTIONS`/`PROPFIND` methods are accepted —
  `PUT`/`DELETE`/`MKCOL`/etc. are rejected with 405.
- Served **only** on the tailnet listener — nothing is bound to public
  interfaces on the VPS.
- Concurrent by default: each accepted connection is handled in its own
  goroutine, so multiple parallel downloads (e.g. from a download manager
  with segmented downloads, or several clients at once) don't block each
  other.

## Mounting as a read-only WebDAV share

The same directory is exposed over read-only WebDAV at
`http://<hostname>:8080/dav/`. Any WebDAV client works — since WebDAV is
read-only, attempts to write return 405.

```sh
# rclone (works on Linux/macOS/Windows)
rclone config create vps webdav url=http://remote-tools-<vps>:8080/dav/ vendor=other
rclone ls vps:

# Linux: mount with davfs2
sudo mount -t davfs http://remote-tools-<vps>:8080/dav/ /mnt/vps

# macOS Finder: Cmd-K, then enter
#   http://remote-tools-<vps>:8080/dav/

# Windows Explorer:
net use Z: http://remote-tools-<vps>:8080/dav/
```

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
