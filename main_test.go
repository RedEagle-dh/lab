package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardRefuses(t *testing.T) {
	rules := builtinGuards
	for _, cmd := range []string{
		"zfs destroy storage/apps/x",
		"zfs rollback storage/apps/media@x",
		"zpool create -f tank /dev/sdb",
		"mkfs.ext4 /dev/sdc1",
		"dd if=/dev/zero of=/dev/sda bs=1M",
		"rm -rf /home/dave/media",
		"cd /x && rm -r config",
		"rm -f -r /srv/data",
		`rm -rf "$tmp"`,
		"docker compose down -v",
		"cd ~/media && docker compose up -d --remove-orphans",
		"docker volume prune -f",
		"docker rm -f portainer",
		"reboot",
		"sleep 5; sudo shutdown -h now",
		"systemctl reboot",
		"systemctl stop ssh",
		"systemctl disable sshd.service",
		"nft flush ruleset",
		"apt-get -y purge nginx",
		"git reset --hard origin/main",
		"git push -f origin main",
		"crontab -r",
	} {
		if _, _, hit := guardHit(rules, cmd); !hit {
			t.Errorf("not refused: %s", cmd)
		}
	}
}

func TestGuardAllows(t *testing.T) {
	rules := builtinGuards
	for _, cmd := range []string{
		"zfs list -r storage",
		"zpool status",
		"docker compose up -d",
		"docker compose down",
		"docker ps -a",
		"rm -f /etc/foo.conf",
		"rm -rf /tmp/lab-check",
		`rm -rf "/var/tmp/x" /tmp/y`,
		"sudo -u dave git rm -r -q --cached tailscale",
		"cat /var/run/reboot-required",
		"echo reboot needed",
		"systemctl status ssh",
		"systemctl stop sshguard",
		"git status && git log --oneline -5",
		"apt list --upgradable",
		"journalctl -u docker --since -1h | grep -i error",
	} {
		if name, m, hit := guardHit(rules, cmd); hit {
			t.Errorf("refused %q (%s: %q)", cmd, name, m)
		}
	}
}

// The shell wrapper must give commands bash features when bash exists.
func TestShellCmdUsesBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	out, err := exec.Command("sh", "-c", shellCmd(`cat <(echo "it's bash")`)).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "it's bash" {
		t.Fatalf("got %q, %v", out, err)
	}
}

// lab run: the script arrives on stdin, args are passed through, and the script's own
// stdin is /dev/null so it cannot swallow itself.
func TestRunWrapper(t *testing.T) {
	c := exec.Command("sh", "-c", shellCmd(runWrapper([]string{"a b", "it's"})))
	c.Stdin = strings.NewReader("printf '%s|' \"$@\"\nif read -r x; then echo stdin:$x; else echo nostdin; fi\nexit 7\n")
	out, err := c.CombinedOutput()
	if got := string(out); got != "a b|it's|nostdin\n" {
		t.Fatalf("output %q", got)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Fatalf("want exit 7, got %v", err)
	}
}

func TestExpandAndToLocal(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "nodes", "server", "rootfs")
	for _, f := range []string{"etc/a.conf", "etc/b.env.example", "home/dave/x/compose.yml"} {
		p := filepath.Join(rootfs, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(f), 0o644)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(dir, "nodes", "server")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	got, err := findRootfs("server")
	if err != nil {
		t.Fatal(err)
	}
	if real, _ := filepath.EvalSymlinks(got); real != mustEval(t, rootfs) {
		t.Fatalf("rootfs %s", got)
	}

	ps, skipped, err := expand(got, nil)
	if err != nil || skipped != 1 || len(ps) != 2 || ps[0].remote != "/etc/a.conf" || ps[1].remote != "/home/dave/x/compose.yml" {
		t.Fatalf("expand all: %v %d %v", ps, skipped, err)
	}
	for _, arg := range []string{"/etc/a.conf", "rootfs/etc/a.conf", filepath.Join(got, "etc/a.conf")} {
		ps, _, err := expand(got, []string{arg})
		if err != nil || len(ps) != 1 || ps[0].remote != "/etc/a.conf" {
			t.Errorf("%s: %v %v", arg, ps, err)
		}
	}
	if _, err := toLocal(got, "README.md"); err == nil {
		t.Error("relative path outside rootfs accepted")
	}
}

func mustEval(t *testing.T, p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
