# lab

A single static binary that gives an AI agent (e.g. Claude Code) root shell access to every
node in a homelab over SSH, with compact, token-friendly output. Think `kubectl exec`
without running Kubernetes.

```
lab                          node status: load, mem, disk, uptime (one line per node)
lab dns                      overview: os, disks, zpools, failed units, containers, ports
lab server 'zpool status'    run a command as root
lab server/paperless 'ls /data'   run inside a Docker container on that node
lab all 'df -h /'            run on all nodes in parallel (or: lab pi5,dns ...)
lab put app.yml pi5:/etc/app.yml  /  lab get dns:/etc/hosts
```

## Why

- **No connection boilerplate:** the agent never deals with IPs, users, keys or ssh flags.
  Connections are multiplexed (`ControlPersist=10m`), so repeated calls are instant.
- **Token-friendly output:** no colors, pagers or progress bars; output is capped to the
  first 40 + last 160 lines with a hint to filter or use `-f`; overlong lines are cut.
- **Clear results:** `[exit N]`, `[timeout after ...]` and a distinct message when SSH
  itself fails. The default timeout is 2m (`-t 10m`, `-t 0` for none), enforced remotely too.
- **Root everywhere:** direct for `root@` targets, `sudo -n` otherwise.
- **Audit log:** every command with exit code and duration goes to
  `~/.local/state/lab/history.log`.

## Install

Build (needs only Docker, no local Go toolchain):

```sh
./build.sh          # -> dist/lab-linux-amd64, dist/lab-linux-arm64
```

Then on the machine the agent runs on (ideally one that stays up when your main server is down):

```sh
./install.sh
```

This installs `/usr/local/bin/lab`, creates `~/.config/lab/nodes` from `nodes.example`,
generates `~/.ssh/lab_ed25519` and adds a short "use `lab`" note to `~/.claude/CLAUDE.md`.
Authorize the printed public key on each node, edit the node list, then run `lab`.

Optionally allow `Bash(lab:*)` in Claude Code's permissions so the agent doesn't ask each time.

## Configuration

`$LAB_CONFIG`, else `~/.config/lab/nodes`, else `/etc/lab/nodes`:

```
# name   target                 description
server   root@server.home.lan   x86 server: Docker, ZFS
pi5      local                  this node
dns      pi@192.168.1.2:22      Pi-hole only
```

`target` is `local` or `[user@]host[:port]` (user defaults to `root`). `$LAB_KEY` overrides
the SSH key (default `~/.ssh/lab_ed25519`, falling back to your normal SSH setup).

## Security

`lab` intentionally hands out root on every configured node. Anyone (or any agent) that can
run it on the control machine owns the whole lab. Use a dedicated key and keep the control
machine locked down.
