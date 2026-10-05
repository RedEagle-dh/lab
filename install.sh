#!/bin/sh
# Installs lab on this machine (meant for the Pi 5 that runs the agent).
# Usage: ./install.sh            (run as the user the agent runs as, not root)
set -e
cd "$(dirname "$0")"

case "$(uname -m)" in
  aarch64|arm64) bin=dist/lab-linux-arm64 ;;
  x86_64)        bin=dist/lab-linux-amd64 ;;
  *) echo "unsupported arch $(uname -m)"; exit 1 ;;
esac
sudo install -m 755 "$bin" /usr/local/bin/lab

mkdir -p ~/.config/lab
[ -f ~/.config/lab/nodes ] || cp nodes.example ~/.config/lab/nodes

[ -f ~/.ssh/lab_ed25519 ] || ssh-keygen -q -t ed25519 -N '' -C "lab@$(hostname)" -f ~/.ssh/lab_ed25519

# Tell Claude Code about lab (global, loaded every session).
mkdir -p ~/.claude
if ! grep -q '## Homelab access' ~/.claude/CLAUDE.md 2>/dev/null; then
  cat >> ~/.claude/CLAUDE.md <<'MD'

## Homelab access
Use `lab` (not ssh) to inspect and change homelab nodes; commands run as root.
Start with `lab` (node status) or `lab <node>` (overview); `lab -h` shows all usage.
MD
fi

cat <<MSG

lab installed. Next steps:
1. Edit ~/.config/lab/nodes (users, IPs, relay Pi).
2. Authorize this key on every remote node:
     $(cat ~/.ssh/lab_ed25519.pub)
   - root@ targets:  append it to /root/.ssh/authorized_keys on that node
   - user@ targets:  ssh-copy-id -i ~/.ssh/lab_ed25519 user@host  (user needs NOPASSWD sudo)
3. Check:  lab
MSG
