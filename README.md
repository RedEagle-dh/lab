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
lab run server fix.sh arg       run a local script there (bash if available)
lab -u dave server 'git pull'    run as another user instead of root
lab put app.yml pi5:/etc/app.yml  /  lab get dns:/etc/hosts
lab diff server                  compare the repo copies in nodes/server/rootfs with the node
lab deploy server /etc/foo.conf  upload the repo copy if it differs (backup, owner kept)
```

## Why

- **No connection boilerplate:** the agent never deals with IPs, users, keys or ssh flags.
  Connections are multiplexed (`ControlPersist=10m`), so repeated calls are instant.
- **Token-friendly output:** no colors, pagers or progress bars; output is capped to the
  first 40 + last 160 lines with a hint to filter or use `-f`; overlong lines are cut.
- **Clear results:** `[exit N]`, `[timeout after ...]` and a distinct message when SSH
  itself fails. The default timeout is 2m (`-t 10m`, `-t 0` for none), enforced remotely too.
- **Root everywhere:** direct for `root@` targets, `sudo -n` otherwise; `-u <user>` runs as
  someone else (e.g. git in a user's repo without leaving root-owned files behind).
- **No quoting traps:** commands run in bash when the target has it (else sh), and anything
  longer goes through `lab run <node> script.sh [args]`: the script is uploaded to a temp
  file and runs with `/dev/null` as stdin, so commands inside it cannot swallow the script.
- **Safe uploads:** `put` stages the data first, skips identical content, keeps
  `<path>.bak.<YYYY-MM-DD>` of a replaced file and writes in place (owner, mode and inode
  stay, so Docker single-file bind mounts see the change). A new file gets the parent
  directory's owner and the local file's mode. `--no-backup` turns the backup off.
- **Repo as source of truth:** with a `nodes/<node>/rootfs/` tree (found in the current
  directory or a parent), `lab diff <node> [path...]` shows unified diffs between the repo
  and the node in one round trip, and `lab deploy <node> <path...>` uploads what differs,
  then runs `systemctl daemon-reload` if units changed. `*.example` files are skipped.
- **Guard against accidents:** destructive commands (`zfs destroy`, `rm -r` outside /tmp,
  `mkfs`, `compose down -v`, `--remove-orphans`, `docker rm/prune`, reboot/shutdown, stopping
  sshd, firewall flushes, package removal, `git reset --hard`, ...) are refused with exit 3
  unless `-y` is given. Add your own patterns, one regexp per line, in `~/.config/lab/guard`
  (next to the nodes file).
- **Audit log:** every command, upload, refusal and the `-u`/`-y` flags go to
  `~/.local/state/lab/history.log`; every script run with `lab run` is kept by hash in
  `~/.local/state/lab/scripts/`.

## Install

Build (needs only Docker, no local Go toolchain):

```sh
./build.sh          # -> dist/lab-{linux,darwin}-{amd64,arm64}
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
