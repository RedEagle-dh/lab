// lab: run commands as root on homelab nodes over SSH, with agent-friendly
// (compact, capped, color-free) output.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const usage = `lab - run commands as root on homelab nodes

  lab                         list nodes with status (load, mem, disk, uptime)
  lab <node>                  overview: os, disks, failed units, containers, ports
  lab <node> <cmd...>         run shell cmd as root on node
  lab <node>/<ctr> <cmd...>   run cmd inside docker container <ctr> on node
  lab all <cmd...>            run on all nodes in parallel (also: lab a,b <cmd>)
  lab run <node> <script|-> [args...]   run a local script there (any target form above)
  lab put <file|-> <node>:<path>        upload (node/ctr:path works too)
  lab get <node>:<path> [file]          download (default: ./basename)
  lab diff <node> [path...]             compare repo copies (nodes/<node>/rootfs) with the node
  lab deploy <node> <path...>           upload repo copies that differ from the node

flags (before the subcommand or target):
  -t <dur>      timeout (default 2m, 0=none)
  -f            full output, no capping
  -u <user>     run as <user> instead of root (commands and run)
  -y, --yes     allow a command that matches a destructive pattern (see below)
  --no-backup   put/deploy: do not keep a backup of a replaced file

commands run via bash if the target has it, else sh: lab pi5 'journalctl -u x | grep -i err'
more than one line or nested quotes: write a script file and use lab run instead.
stdin is forwarded: lab pi5 'wc -l' < file   (use put for files)
put/deploy write an existing file in place (keeps owner, mode and inode) after backing it up
  as <path>.bak.<YYYY-MM-DD>; unchanged files are skipped; a new file gets its parent
  directory's owner and the local file's mode, new directories the owner of the nearest
  existing one.
diff/deploy map nodes/<node>/rootfs/<path> (current dir or a parent) to <path> on the node;
  give paths as /etc/foo or nodes/<node>/rootfs/etc/foo; *.example files are skipped.
  deploy runs systemctl daemon-reload after changing files under /etc/systemd/.
refused without -y: zfs destroy/rollback, zpool destroy/create/..., rm -r (except in /tmp),
  mkfs, dd of=/dev, compose down -v, --remove-orphans, docker rm/prune, reboot/shutdown,
  stopping sshd, firewall flushes, apt remove/purge, git reset --hard/push -f, ...
  more patterns (one regexp per line): <config dir>/guard
output is capped to first 40 + last 160 lines; non-zero exit prints [exit N].
history: ~/.local/state/lab/history.log, scripts from lab run in ~/.local/state/lab/scripts/
config: $LAB_CONFIG | ~/.config/lab/nodes | /etc/lab/nodes  (name target description)
`

// Remote environment that keeps output plain and non-interactive.
const envPrefix = "export TERM=dumb NO_COLOR=1 PAGER=cat SYSTEMD_PAGER= SYSTEMD_COLORS=0 DEBIAN_FRONTEND=noninteractive; "

// pickShell runs "$1" with bash when the target has it (process substitution, arrays,
// [[ ]]), else with sh. Passing the script as an argument avoids another quoting layer.
const pickShell = `if command -v bash >/dev/null 2>&1; then exec bash -c "$1"; fi; exec sh -c "$1"`

const probeScript = `read l _ < /proc/loadavg
m=$(awk '/^MemTotal/{t=$2}/^MemAvailable/{a=$2}END{printf "%d%%",(t-a)*100/t}' /proc/meminfo)
d=$(df -P / | awk 'NR==2{print $5}')
u=$(awk '{printf "%dd%dh",$1/86400,($1%86400)/3600}' /proc/uptime)
echo "$l $m $d $u"`

const infoScript = `. /etc/os-release 2>/dev/null
echo "host: $(hostname)  os: $PRETTY_NAME  kernel: $(uname -r) $(uname -m)"
echo "uptime: $(uptime -p 2>/dev/null | sed 's/^up //')  load: $(cut -d' ' -f1-3 /proc/loadavg)"
free -h | awk 'NR==2{print "mem: "$3" used / "$2", "$7" avail"}'
t=/sys/class/thermal/thermal_zone0/temp; [ -r $t ] && awk '{printf "temp: %.1fC\n",$1/1000}' $t
echo "disks:"; df -hP -x tmpfs -x devtmpfs -x overlay -x squashfs -x efivarfs 2>/dev/null | awk 'NR>1{print "  "$6" "$3"/"$2" ("$5")"}'
command -v zpool >/dev/null && zpool list -H -o name,health,cap 2>/dev/null | awk '{print "zpool: "$1" "$2" "$3}'
f=$(systemctl --failed --no-legend --plain 2>/dev/null | awk '{print $1}' | tr '\n' ' '); echo "failed units: ${f:-none}"
command -v docker >/dev/null && { echo "containers:"; docker ps -a --format '  {{.Names}}: {{.Status}}' 2>/dev/null | sort; }
echo "listening tcp: $(ss -tlnH 2>/dev/null | awk '{n=split($4,a,":"); print a[n]}' | sort -nu | tr '\n' ' ')"`

// putScript writes stdin to $1. It stages the data in a temp file first, so a broken
// transfer never truncates the target, then skips identical content, backs up the old
// file and writes in place: owner, mode and inode stay (Docker single-file bind mounts
// follow the inode). A new file gets the parent directory's owner and mode $2; new
// directories get the owner of the nearest existing ancestor instead of root.
const putScript = `p=$1; m=$2; nb=$3
d=$(dirname "$p")
a=$d; while [ ! -e "$a" ]; do a=$(dirname "$a"); done
if [ "$a" != "$d" ]; then
  o=$(stat -c %u:%g "$a")
  mkdir -p "$d" || exit 1
  x=$d; while [ "$x" != "$a" ]; do chown "$o" "$x" || exit 1; x=$(dirname "$x"); done
  echo "lab-put mkdir $d"
fi
t=$(mktemp) || exit 1
trap 'rm -f "$t"' EXIT
cat > "$t" || exit 1
if [ -e "$p" ]; then
  if command -v cmp >/dev/null 2>&1 && cmp -s "$t" "$p"; then echo "lab-put unchanged"; exit 0; fi
  if [ "$nb" != 1 ]; then
    b="$p.bak.$(date +%F)"; [ -e "$b" ] && b="$b-$(date +%H%M%S)"
    cp -a "$p" "$b" || exit 1
    echo "lab-put backup $b"
  fi
  cat "$t" > "$p" || exit 1
  echo "lab-put updated"
else
  cat "$t" > "$p" && chmod "$m" "$p" && chown "$(stat -c %u:%g "$d")" "$p" || exit 1
  echo "lab-put created $(stat -c %U:%G "$p")"
fi`

type node struct {
	name, user, host, port, desc string
	local                        bool
}

type target struct {
	node      node
	container string
}

type opts struct {
	timeout  time.Duration
	full     bool
	user     string
	yes      bool
	noBackup bool
}

var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	o := opts{timeout: 2 * time.Minute}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "-h", "--help":
			fmt.Print(usage)
			return 0
		case "-f", "--full":
			o.full = true
			args = args[1:]
		case "-y", "--yes":
			o.yes = true
			args = args[1:]
		case "--no-backup":
			o.noBackup = true
			args = args[1:]
		case "-u", "--user":
			if len(args) < 2 || !userRe.MatchString(args[1]) {
				return fail("-u needs a user name")
			}
			o.user = args[1]
			args = args[2:]
		case "-t", "--timeout":
			if len(args) < 2 {
				return fail("-t needs a duration (e.g. 30s, 10m, 0)")
			}
			d, err := parseDur(args[1])
			if err != nil {
				return fail("bad timeout %q", args[1])
			}
			o.timeout = d
			args = args[2:]
		default:
			return fail("unknown flag %s (see lab -h)", args[0])
		}
	}

	nodes, err := loadConfig()
	if err != nil {
		return fail("%v", err)
	}

	if len(args) == 0 || args[0] == "ls" {
		return list(nodes)
	}
	switch args[0] {
	case "help":
		fmt.Print(usage)
		return 0
	case "info":
		if len(args) != 2 {
			return fail("usage: lab info <node>")
		}
		args = args[1:]
	case "put":
		if len(args) != 3 {
			return fail("usage: lab put <file|-> <node>:<path>")
		}
		if o.user != "" {
			return fail("-u does not apply to put: a new file gets its parent directory's owner")
		}
		return put(nodes, args[1], args[2], o)
	case "get":
		if len(args) < 2 || len(args) > 3 {
			return fail("usage: lab get <node>:<path> [file]")
		}
		return get(nodes, args[1:], o)
	case "run":
		if len(args) < 3 {
			return fail("usage: lab run <node> <script|-> [args...]")
		}
		return runScript(nodes, args[1], args[2], args[3:], o)
	case "diff":
		if len(args) < 2 {
			return fail("usage: lab diff <node> [path...]")
		}
		return sync1(nodes, args[1], args[2:], o, false)
	case "deploy":
		if len(args) < 3 {
			return fail("usage: lab deploy <node> <path...>  (lab diff <node> shows what differs)")
		}
		if o.user != "" {
			return fail("-u does not apply to deploy: a new file gets its parent directory's owner")
		}
		return sync1(nodes, args[1], args[2:], o, true)
	}

	targets, err := resolve(nodes, args[0])
	if err != nil {
		return fail("%v", err)
	}
	cmd := strings.Join(args[1:], " ")
	if cmd == "" {
		cmd = infoScript
	} else if refused(targets, cmd, cmd, o) {
		return 3
	}
	if len(targets) == 1 {
		var in io.Reader
		if stdinIsData() {
			in = os.Stdin
		}
		return single(targets[0], cmd, o, in, "")
	}
	return multi(targets, cmd, o, nil, "")
}

// ---------- config ----------

func configPaths(file string) []string {
	if p := os.Getenv("LAB_CONFIG"); p != "" {
		if file == "nodes" {
			return []string{p}
		}
		return []string{filepath.Join(filepath.Dir(p), file)}
	}
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, ".config/lab", file), filepath.Join("/etc/lab", file)}
}

func loadConfig() ([]node, error) {
	paths := configPaths("nodes")
	for _, p := range paths {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return parseConfig(f, p)
	}
	return nil, fmt.Errorf("no config found (tried %s)", strings.Join(paths, ", "))
}

func parseConfig(r io.Reader, path string) ([]node, error) {
	var nodes []node
	sc := bufio.NewScanner(r)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s:%d: expected '<name> <target> [description]'", path, ln)
		}
		n := node{name: fields[0], desc: strings.Join(fields[2:], " ")}
		if reserved[n.name] || strings.ContainsAny(n.name, "/,:") {
			return nil, fmt.Errorf("%s:%d: invalid node name %q", path, ln, n.name)
		}
		t := fields[1]
		if t == "local" {
			n.local = true
		} else {
			if i := strings.Index(t, "@"); i >= 0 {
				n.user, t = t[:i], t[i+1:]
			}
			if i := strings.LastIndex(t, ":"); i >= 0 && !strings.Contains(t[i+1:], "]") {
				n.port, t = t[i+1:], t[:i]
			}
			n.host = t
			if n.user == "" {
				n.user = "root"
			}
		}
		nodes = append(nodes, n)
	}
	return nodes, sc.Err()
}

var reserved = map[string]bool{"all": true, "ls": true, "help": true, "info": true,
	"put": true, "get": true, "run": true, "diff": true, "deploy": true}

func resolve(nodes []node, spec string) ([]target, error) {
	if spec == "all" {
		ts := make([]target, len(nodes))
		for i, n := range nodes {
			ts[i] = target{node: n}
		}
		return ts, nil
	}
	var ts []target
	for _, part := range strings.Split(spec, ",") {
		name, ctr, _ := strings.Cut(part, "/")
		n, ok := find(nodes, name)
		if !ok {
			return nil, fmt.Errorf("unknown node %q (known: %s)", name, names(nodes))
		}
		ts = append(ts, target{node: n, container: ctr})
	}
	return ts, nil
}

func find(nodes []node, name string) (node, bool) {
	for _, n := range nodes {
		if n.name == name {
			return n, true
		}
	}
	return node{}, false
}

func names(nodes []node) string {
	var s []string
	for _, n := range nodes {
		s = append(s, n.name)
	}
	return strings.Join(s, ", ")
}

func (t target) String() string {
	if t.container != "" {
		return t.node.name + "/" + t.container
	}
	return t.node.name
}

// ---------- execution ----------

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellCmd is the shell invocation for script: bash if present, else sh.
func shellCmd(script string) string {
	return "sh -c " + shQuote(pickShell) + " lab " + shQuote(script)
}

// remoteCommand builds the root shell command for a target. withStdin adds
// docker exec -i so stdin reaches the container.
func remoteCommand(t target, cmd string, o opts, withStdin bool) string {
	inner := envPrefix + cmd
	var c string
	if t.container != "" {
		flags := ""
		if withStdin {
			flags += "-i "
		}
		if o.user != "" {
			flags += "-u " + shQuote(o.user) + " "
		}
		c = "docker exec " + flags + shQuote(t.container) + " " + shellCmd(inner)
	} else {
		c = shellCmd(inner)
		if o.user != "" {
			c = "sudo -n -H -u " + shQuote(o.user) + " " + c
		}
	}
	if o.timeout > 0 {
		c = fmt.Sprintf("timeout -k 5 %d %s", int(o.timeout.Seconds()), c)
	}
	if needsSudo(t.node) {
		c = "sudo -n " + c
	}
	return c
}

func needsSudo(n node) bool {
	if n.local {
		return os.Geteuid() != 0
	}
	return n.user != "root"
}

func command(ctx context.Context, n node, remote string, stdin bool) *exec.Cmd {
	if n.local {
		return exec.CommandContext(ctx, "sh", "-c", remote)
	}
	home, _ := os.UserHomeDir()
	cm := filepath.Join(home, ".cache/lab")
	os.MkdirAll(cm, 0o700)
	a := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=15",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "LogLevel=ERROR",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + filepath.Join(cm, "%C"),
		"-o", "ControlPersist=10m",
	}
	key := os.Getenv("LAB_KEY")
	if key == "" {
		key = filepath.Join(home, ".ssh/lab_ed25519")
	}
	if _, err := os.Stat(key); err == nil {
		a = append(a, "-i", key)
	}
	if n.port != "" {
		a = append(a, "-p", n.port)
	}
	if !stdin {
		a = append(a, "-n")
	}
	a = append(a, n.user+"@"+n.host, "--", remote)
	return exec.CommandContext(ctx, "ssh", a...)
}

func stdinIsData() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	m := fi.Mode()
	return m&os.ModeNamedPipe != 0 || m.IsRegular()
}

type result struct {
	out  string
	code int
	dur  time.Duration
}

// exec1 runs cmd on t. label is what goes into the history log (default: cmd).
func exec1(t target, cmd string, o opts, stdin io.Reader, label string) result {
	ctx := context.Background()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout+15*time.Second)
		defer cancel()
	}
	c := command(ctx, t.node, remoteCommand(t, cmd, o, stdin != nil), stdin != nil)
	if stdin != nil {
		c.Stdin = stdin
	}
	w := newCapWriter(o.full)
	c.Stdout, c.Stderr = w, w
	start := time.Now()
	err := c.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			code = ee.ExitCode()
		} else {
			code = 1
			fmt.Fprintf(w, "%v\n", err)
		}
	}
	res := result{out: w.String(), code: code, dur: time.Since(start)}
	if label == "" {
		label = cmd
	}
	logHistory(t, label, o, res)
	return res
}

func status(r result, o opts, t target) string {
	switch {
	case r.code == 0:
		return ""
	case r.code == 124 || (o.timeout > 0 && r.dur >= o.timeout):
		return fmt.Sprintf("[timeout after %s; use -t to raise]", o.timeout)
	case r.code == 255 && !t.node.local:
		return "[exit 255: ssh failed - node down or key not authorized?]"
	default:
		return fmt.Sprintf("[exit %d]", r.code)
	}
}

func single(t target, cmd string, o opts, in io.Reader, label string) int {
	r := exec1(t, cmd, o, in, label)
	fmt.Print(r.out)
	if s := status(r, o, t); s != "" {
		fmt.Println(s)
	}
	return r.code
}

// multi runs cmd on all targets in parallel; each gets its own copy of stdin (may be nil).
func multi(ts []target, cmd string, o opts, stdin []byte, label string) int {
	res := make([]result, len(ts))
	var wg sync.WaitGroup
	for i, t := range ts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var in io.Reader
			if stdin != nil {
				in = bytes.NewReader(stdin)
			}
			res[i] = exec1(t, cmd, o, in, label)
		}()
	}
	wg.Wait()
	rc := 0
	for i, t := range ts {
		h := "== " + t.String()
		if s := status(res[i], o, t); s != "" {
			h += " " + s
			rc = 1
		}
		fmt.Println(h + " ==")
		fmt.Print(res[i].out)
	}
	return rc
}

// runWrapper uploads stdin (the script) to a temp file and runs it with the given args.
// The script gets /dev/null as stdin so commands in it cannot swallow the script itself.
func runWrapper(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shQuote(a)
	}
	a := strings.Join(q, " ")
	return `t=$(mktemp) || exit 1; cat > "$t"; ` +
		`if command -v bash >/dev/null 2>&1; then bash "$t" ` + a + ` </dev/null; else sh "$t" ` + a + ` </dev/null; fi; ` +
		`rc=$?; rm -f "$t"; exit $rc`
}

func runScript(nodes []node, spec, file string, args []string, o opts) int {
	var script []byte
	var err error
	name := file
	if file == "-" {
		script, err = io.ReadAll(os.Stdin)
		name = "stdin"
	} else {
		script, err = os.ReadFile(file)
	}
	if err != nil {
		return fail("%v", err)
	}
	targets, err := resolve(nodes, spec)
	if err != nil {
		return fail("%v", err)
	}
	sum := sha256.Sum256(script)
	id := hex.EncodeToString(sum[:])[:12]
	label := fmt.Sprintf("run %s sha=%s", name, id)
	if len(args) > 0 {
		label += " " + strings.Join(args, " ")
	}
	if refused(targets, string(script), label, o) {
		return 3
	}
	saveScript(id, script)
	cmd := runWrapper(args)
	if len(targets) == 1 {
		return single(targets[0], cmd, o, bytes.NewReader(script), label)
	}
	return multi(targets, cmd, o, script, label)
}

func list(nodes []node) int {
	o := opts{timeout: 8 * time.Second}
	res := make([]result, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i] = exec1(target{node: n}, probeScript, o, nil, "")
		}()
	}
	wg.Wait()
	rows := [][]string{{"NODE", "STATE", "LOAD", "MEM", "DISK/", "UP", "DESCRIPTION"}}
	var errs []string
	for i, n := range nodes {
		f := strings.Fields(res[i].out)
		if res[i].code == 0 && len(f) == 4 {
			rows = append(rows, []string{n.name, "up", f[0], f[1], f[2], f[3], n.desc})
			continue
		}
		rows = append(rows, []string{n.name, "DOWN", "-", "-", "-", "-", n.desc})
		reason := strings.TrimSpace(res[i].out)
		if j := strings.LastIndexByte(reason, '\n'); j >= 0 {
			reason = reason[j+1:]
		}
		if reason == "" {
			reason = status(res[i], o, target{node: n})
		}
		where := "local"
		if !n.local {
			where = n.user + "@" + n.host
		}
		errs = append(errs, fmt.Sprintf("%s (%s): %s", n.name, where, reason))
	}
	printTable(rows)
	if len(errs) > 0 {
		fmt.Println()
		for _, e := range errs {
			fmt.Println(e)
		}
	}
	return 0
}

func printTable(rows [][]string) {
	w := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			w[i] = max(w[i], len(c))
		}
	}
	for _, r := range rows {
		var b strings.Builder
		for i, c := range r {
			if i == len(r)-1 {
				b.WriteString(c)
			} else {
				fmt.Fprintf(&b, "%-*s  ", w[i], c)
			}
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
}

// ---------- destructive command guard ----------

type guardRule struct {
	name string
	re   *regexp.Regexp
	// allow, if set, can wave through a specific match (loc = match indexes in text).
	allow func(text string, loc []int) bool
}

var builtinGuards = []guardRule{
	{name: "zfs destroy/rollback", re: regexp.MustCompile(`\bzfs\s+(?:destroy|rollback)\b`)},
	{name: "zpool destroy/create/detach/remove", re: regexp.MustCompile(`\bzpool\s+(?:destroy|labelclear|create|detach|remove|split)\b`)},
	{name: "filesystem/disk wipe", re: regexp.MustCompile(`\b(?:mkfs(?:\.\w+)?|wipefs|blkdiscard|mkswap|shred)\b`)},
	{name: "dd to a device", re: regexp.MustCompile(`\bdd\b[^|;&\n]*\bof=/dev/`)},
	{name: "write to a block device", re: regexp.MustCompile(`>\s*/dev/(?:sd|nvme|mmcblk|hd|vd)`)},
	{name: "partition table change", re: regexp.MustCompile(`\bsgdisk\b[^|;&\n]*\s(?:-Z|--zap-all|-o|--clear)\b|\bparted\b[^|;&\n]*\b(?:mklabel|rm)\b`)},
	{name: "recursive rm", re: regexp.MustCompile(`\brm\s+(?:-{1,2}[\w-]+\s+)*(?:-\w*[rR]\w*|--recursive)\b`), allow: rmAllowed},
	{name: "compose down -v (deletes volumes)", re: regexp.MustCompile(`\b(?:compose|docker-compose)\b[^|;&\n]*\bdown\b[^|;&\n]*\s(?:-v|--volumes)\b`)},
	{name: "--remove-orphans", re: regexp.MustCompile(`--remove-orphans\b`)},
	{name: "docker rm/prune", re: regexp.MustCompile(`\bdocker\s+(?:(?:volume|system|image|container|network|builder)\s+(?:rm|prune|remove)|rm|rmi)\b`)},
	{name: "reboot/shutdown", re: regexp.MustCompile(`(?m)(?:^\s*|[;&|(]\s*|\bsudo\s+)(?:reboot|poweroff|halt|shutdown)(?:\s|;|$)|\bsystemctl\s+(?:reboot|poweroff|halt|kexec)\b`)},
	{name: "stopping ssh", re: regexp.MustCompile(`\bsystemctl\s+(?:stop|disable|mask|kill)\b[^|;&\n]*\bsshd?(?:\.service|\.socket)?(?:\s|;|$)`)},
	{name: "firewall flush", re: regexp.MustCompile(`\bnft\s+flush\s+ruleset\b|\biptables\s+(?:-F|--flush|-X)\b|\bufw\s+(?:enable|reset)\b`)},
	{name: "package removal", re: regexp.MustCompile(`\b(?:apt|apt-get|aptitude)\s+(?:-\S+\s+)*(?:remove|purge|autoremove)\b|\bdpkg\s+(?:-r|-P|--remove|--purge)\b`)},
	{name: "destructive git", re: regexp.MustCompile(`\bgit\b[^|;&\n]*\s(?:reset\s+--hard\b|clean\s+-\w*f|branch\s+-D\b|push\b[^|;&\n]*\s(?:--force(?:-with-lease)?|-f)\b)`)},
	{name: "crontab -r", re: regexp.MustCompile(`\bcrontab\s+(?:-\w+\s+)*-r\b`)},
	{name: "userdel", re: regexp.MustCompile(`\buserdel\b`)},
}

var gitRmRe = regexp.MustCompile(`\bgit(?:\s+-C\s+\S+)?\s+$`)

// rmAllowed lets "git rm -r" (only touches the index/work tree of a repo) and recursive
// removal of paths under /tmp or /var/tmp through.
func rmAllowed(text string, loc []int) bool {
	if gitRmRe.MatchString(text[:loc[0]]) {
		return true
	}
	rest := text[loc[1]:]
	if i := strings.IndexAny(rest, ";&|\n"); i >= 0 {
		rest = rest[:i]
	}
	paths := 0
	for _, f := range strings.Fields(rest) {
		if strings.HasPrefix(f, "-") {
			continue
		}
		f = strings.Trim(f, `"'`)
		if !strings.HasPrefix(f, "/tmp/") && !strings.HasPrefix(f, "/var/tmp/") {
			return false
		}
		paths++
	}
	return paths > 0
}

func loadGuards() []guardRule {
	rules := builtinGuards
	for _, p := range configPaths("guard") {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for ln, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			re, err := regexp.Compile(line)
			if err != nil {
				fmt.Fprintf(os.Stderr, "lab: %s:%d: bad pattern ignored: %v\n", p, ln+1, err)
				continue
			}
			rules = append(rules, guardRule{name: line, re: re})
		}
		break
	}
	return rules
}

// guardHit returns the first rule matching text and the matched snippet.
func guardHit(rules []guardRule, text string) (string, string, bool) {
	for _, r := range rules {
		for _, loc := range r.re.FindAllStringIndex(text, -1) {
			if r.allow != nil && r.allow(text, loc) {
				continue
			}
			return r.name, strings.TrimSpace(text[loc[0]:loc[1]]), true
		}
	}
	return "", "", false
}

// refused reports (and logs) a destructive command that was not confirmed with -y.
func refused(ts []target, text, label string, o opts) bool {
	if o.yes {
		return false
	}
	name, m, hit := guardHit(loadGuards(), text)
	if !hit {
		return false
	}
	fmt.Fprintf(os.Stderr, "lab: refused (%s): %q\n", name, m)
	fmt.Fprintln(os.Stderr, "lab: destructive pattern; confirm with the user, then re-run with -y before the node")
	for _, t := range ts {
		logHistory(t, "REFUSED "+label, o, result{code: 3})
	}
	return true
}

// ---------- file transfer ----------

func splitRemote(nodes []node, spec string) (target, string, error) {
	tspec, path, ok := strings.Cut(spec, ":")
	if !ok || path == "" {
		return target{}, "", fmt.Errorf("expected <node>:<path>, got %q", spec)
	}
	ts, err := resolve(nodes, tspec)
	if err != nil {
		return target{}, "", err
	}
	if len(ts) != 1 {
		return target{}, "", fmt.Errorf("put/get need exactly one node")
	}
	return ts[0], path, nil
}

type putResult struct {
	state  string // created, updated, unchanged
	backup string
	owner  string
	mkdir  string // directory that had to be created, if any
}

// putTo writes data to path on t with putScript semantics.
func putTo(t target, path string, data io.Reader, mode fs.FileMode, o opts, label string) (putResult, result) {
	nb := "0"
	if o.noBackup {
		nb = "1"
	}
	cmd := fmt.Sprintf("set -- %s %o %s\n%s", shQuote(path), mode.Perm(), nb, putScript)
	r := exec1(t, cmd, o, data, label)
	var pr putResult
	var rest []string
	for _, l := range strings.Split(strings.TrimRight(r.out, "\n"), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "lab-put" {
			switch f[1] {
			case "backup":
				pr.backup = strings.TrimPrefix(l, "lab-put backup ")
			case "mkdir":
				pr.mkdir = strings.TrimPrefix(l, "lab-put mkdir ")
			case "created":
				pr.state = "created"
				if len(f) > 2 {
					pr.owner = f[2]
				}
			default:
				pr.state = f[1]
			}
			continue
		}
		if l != "" {
			rest = append(rest, l)
		}
	}
	r.out = ""
	if len(rest) > 0 {
		r.out = strings.Join(rest, "\n") + "\n"
	}
	return pr, r
}

func describePut(pr putResult, where string, n int64, mode fs.FileMode) string {
	switch pr.state {
	case "unchanged":
		return fmt.Sprintf("unchanged: %s (identical, %d bytes)", where, n)
	case "created":
		s := fmt.Sprintf("created %s (%d bytes, owner %s, mode %o)", where, n, pr.owner, mode.Perm())
		if pr.mkdir != "" {
			s += ", new dir " + pr.mkdir
		}
		return s
	default:
		b := "no backup"
		if pr.backup != "" {
			b = "backup: " + pr.backup
		}
		return fmt.Sprintf("wrote %d bytes to %s (%s)", n, where, b)
	}
}

func put(nodes []node, src, dst string, o opts) int {
	t, path, err := splitRemote(nodes, dst)
	if err != nil {
		return fail("%v", err)
	}
	var in io.Reader = os.Stdin
	mode := fs.FileMode(0o644)
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return fail("%v", err)
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil {
			mode = fi.Mode().Perm()
		}
		in = f
	}
	cr := &countReader{r: in}
	pr, r := putTo(t, path, cr, mode, o, fmt.Sprintf("put %s -> %s", src, path))
	fmt.Print(r.out)
	if r.code != 0 {
		fmt.Println(status(r, o, t))
		return r.code
	}
	fmt.Println(describePut(pr, dst, cr.n, mode))
	return 0
}

// fetch reads a remote file unmodified (no output capping).
func fetch(t target, path string, o opts) ([]byte, error) {
	ctx := context.Background()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout+15*time.Second)
		defer cancel()
	}
	var out, errb bytes.Buffer
	c := command(ctx, t.node, remoteCommand(t, "cat "+shQuote(path), o, false), false)
	c.Stdout, c.Stderr = &out, &errb
	start := time.Now()
	err := c.Run()
	code := 0
	if err != nil {
		code = 1
	}
	logHistory(t, "get "+path, o, result{code: code, dur: time.Since(start)})
	if err != nil {
		return nil, fmt.Errorf("%s%v", errb.String(), err)
	}
	return out.Bytes(), nil
}

func get(nodes []node, args []string, o opts) int {
	t, path, err := splitRemote(nodes, args[0])
	if err != nil {
		return fail("%v", err)
	}
	dst := filepath.Base(path)
	if len(args) == 2 {
		dst = args[1]
	}
	data, err := fetch(t, path, o)
	if err != nil {
		return fail("%v", err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("saved %d bytes to %s\n", len(data), dst)
	return 0
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ---------- diff / deploy against a repo rootfs ----------

type pair struct{ local, remote string }

// findRootfs looks for nodes/<node>/rootfs in the current directory and its parents.
func findRootfs(node string) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, "nodes", node, "rootfs")
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no nodes/%s/rootfs in the current directory or its parents", node)
		}
		dir = parent
	}
}

// toLocal maps a path argument to its file under rootfs. It accepts a repo path
// (relative or absolute, inside rootfs) or the absolute path on the node.
func toLocal(rootfs, arg string) (string, error) {
	abs, err := filepath.Abs(arg)
	if err == nil && (abs == rootfs || strings.HasPrefix(abs, rootfs+string(filepath.Separator))) {
		return abs, nil
	}
	if strings.HasPrefix(arg, "/") {
		return filepath.Join(rootfs, arg), nil
	}
	return "", fmt.Errorf("%s: neither a node path (/etc/...) nor inside %s", arg, rootfs)
}

// expand turns path arguments (files or directories) into local/remote pairs.
// *.example files are skipped: the node has the real file with secrets.
func expand(rootfs string, args []string) ([]pair, int, error) {
	roots := []string{rootfs}
	if len(args) > 0 {
		roots = nil
		for _, a := range args {
			l, err := toLocal(rootfs, a)
			if err != nil {
				return nil, 0, err
			}
			roots = append(roots, l)
		}
	}
	seen := map[string]bool{}
	var ps []pair
	skipped := 0
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			return nil, 0, fmt.Errorf("%s: not in the repo", root)
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if strings.HasSuffix(p, ".example") {
				skipped++
				return nil
			}
			rel, err := filepath.Rel(rootfs, p)
			if err != nil {
				return err
			}
			if !seen[p] {
				seen[p] = true
				ps = append(ps, pair{local: p, remote: "/" + filepath.ToSlash(rel)})
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].remote < ps[j].remote })
	return ps, skipped, nil
}

// remoteHashes returns sha256 per path in one round trip ("MISSING" if absent).
func remoteHashes(t target, paths []string, o opts) (map[string]string, error) {
	var b strings.Builder
	b.WriteString("for f in")
	for _, p := range paths {
		b.WriteString(" " + shQuote(p))
	}
	b.WriteString(`; do if [ -f "$f" ]; then sha256sum "$f"; else echo "MISSING  $f"; fi; done`)
	o.full = true
	r := exec1(t, b.String(), o, nil, fmt.Sprintf("diff: hash %d files", len(paths)))
	if r.code != 0 {
		return nil, fmt.Errorf("%s%s", r.out, status(r, o, t))
	}
	m := map[string]string{}
	for _, l := range strings.Split(r.out, "\n") {
		if h, p, ok := strings.Cut(l, "  "); ok {
			m[p] = h
		}
	}
	return m, nil
}

// unifiedDiff runs the local diff tool: node content (old) against the repo file (new).
func unifiedDiff(remote []byte, local, remotePath, nodeName string) string {
	f, err := os.CreateTemp("", "lab-diff-*")
	if err != nil {
		return err.Error() + "\n"
	}
	defer os.Remove(f.Name())
	f.Write(remote)
	f.Close()
	out, _ := exec.Command("diff", "-u", "-L", nodeName+":"+remotePath, "-L", "repo", f.Name(), local).CombinedOutput()
	return string(out)
}

// sync1 implements diff (apply=false) and deploy (apply=true) for one node.
func sync1(nodes []node, spec string, args []string, o opts, apply bool) int {
	ts, err := resolve(nodes, spec)
	if err != nil {
		return fail("%v", err)
	}
	if len(ts) != 1 || ts[0].container != "" {
		return fail("diff/deploy need exactly one node (no container)")
	}
	t := ts[0]
	rootfs, err := findRootfs(t.node.name)
	if err != nil {
		return fail("%v", err)
	}
	pairs, skipped, err := expand(rootfs, args)
	if err != nil {
		return fail("%v", err)
	}
	if len(pairs) == 0 {
		return fail("no files to compare (only .example files?)")
	}
	remotes := make([]string, len(pairs))
	for i, p := range pairs {
		remotes[i] = p.remote
	}
	hashes, err := remoteHashes(t, remotes, o)
	if err != nil {
		return fail("%v", err)
	}

	same, changed, missing, failed := 0, 0, 0, 0
	updated, created := 0, 0
	reload := false
	for _, p := range pairs {
		data, err := os.ReadFile(p.local)
		if err != nil {
			return fail("%v", err)
		}
		sum := sha256.Sum256(data)
		rh := hashes[p.remote]
		switch {
		case rh == hex.EncodeToString(sum[:]):
			same++
			continue
		case rh == "" || rh == "MISSING":
			missing++
			fmt.Printf("== %s: missing on %s (new file, %d bytes)\n", p.remote, t.node.name, len(data))
		default:
			changed++
			fmt.Printf("== %s\n", p.remote)
			rd, err := fetch(t, p.remote, o)
			if err != nil {
				fmt.Printf("cannot read it: %v\n", err)
			} else {
				w := newCapWriter(o.full)
				w.Write([]byte(unifiedDiff(rd, p.local, p.remote, t.node.name)))
				fmt.Print(w.String())
			}
		}
		if !apply {
			continue
		}
		mode := fs.FileMode(0o644)
		if fi, err := os.Stat(p.local); err == nil {
			mode = fi.Mode().Perm()
		}
		pr, r := putTo(t, p.remote, bytes.NewReader(data), mode, o, "deploy "+p.remote)
		fmt.Print(r.out)
		if r.code != 0 {
			fmt.Println(status(r, o, t))
			failed++
			continue
		}
		fmt.Println("-> " + describePut(pr, t.node.name+":"+p.remote, int64(len(data)), mode))
		switch pr.state {
		case "created":
			created++
		case "updated":
			updated++
		}
		if strings.HasPrefix(p.remote, "/etc/systemd/") && pr.state != "unchanged" {
			reload = true
		}
	}

	sum := fmt.Sprintf("%d identical, %d changed, %d missing on %s", same, changed, missing, t.node.name)
	if apply {
		sum = fmt.Sprintf("%d identical, %d updated, %d created on %s", same, updated, created, t.node.name)
		if failed > 0 {
			sum += fmt.Sprintf(", %d FAILED", failed)
		}
	}
	if skipped > 0 {
		sum += fmt.Sprintf(", %d .example skipped", skipped)
	}
	fmt.Println(sum)
	if reload {
		r := exec1(t, "systemctl daemon-reload", o, nil, "")
		fmt.Print(r.out)
		if s := status(r, o, t); s != "" {
			fmt.Println("systemctl daemon-reload: " + s)
			failed++
		} else {
			fmt.Println("systemctl daemon-reload: done")
		}
	}
	switch {
	case failed > 0:
		return 1
	case !apply && changed+missing > 0:
		return 1
	}
	return 0
}

// ---------- output capping ----------

const (
	headLines = 40
	tailLines = 160
	maxLine   = 400
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-B]`)

// capWriter keeps the first and last lines of output, strips ANSI codes and
// carriage-return progress output, and shortens very long lines.
type capWriter struct {
	mu      sync.Mutex
	full    bool
	head    []string
	tail    []string
	total   int
	partial []byte
}

func newCapWriter(full bool) *capWriter { return &capWriter{full: full} }

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.add(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
	}
	return len(p), nil
}

func (w *capWriter) add(l string) {
	l = ansiRe.ReplaceAllString(l, "")
	if i := strings.LastIndexByte(strings.TrimRight(l, "\r"), '\r'); i >= 0 {
		l = l[i+1:]
	}
	l = strings.TrimRight(l, "\r")
	w.total++
	if w.full {
		w.head = append(w.head, l)
		return
	}
	if len(l) > maxLine {
		l = l[:maxLine] + "…(+" + strconv.Itoa(len(l)-maxLine) + " chars)"
	}
	if len(w.head) < headLines {
		w.head = append(w.head, l)
		return
	}
	w.tail = append(w.tail, l)
	if len(w.tail) > tailLines {
		w.tail = w.tail[1:]
	}
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		w.add(string(w.partial))
		w.partial = nil
	}
	var b strings.Builder
	for _, l := range w.head {
		b.WriteString(l + "\n")
	}
	if skipped := w.total - len(w.head) - len(w.tail); skipped > 0 {
		fmt.Fprintf(&b, "… %d lines omitted (filter with grep/tail, or -f for full output) …\n", skipped)
	}
	for _, l := range w.tail {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// ---------- misc ----------

func stateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local/state/lab")
}

func logHistory(t target, label string, o opts, r result) {
	if label == probeScript {
		return
	}
	if label == infoScript {
		label = "<info>"
	}
	dir := stateDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "history.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	var flags string
	if o.user != "" {
		flags += "[as " + o.user + "] "
	}
	if o.yes {
		flags += "[-y] "
	}
	fmt.Fprintf(f, "%s %s exit=%d dur=%s %s%s\n", time.Now().Format(time.RFC3339), t, r.code,
		r.dur.Round(time.Millisecond), flags, strings.ReplaceAll(label, "\n", "\\n"))
}

// saveScript keeps a copy of every script run with lab run, named by its hash.
func saveScript(id string, script []byte) {
	dir := filepath.Join(stateDir(), "scripts")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	p := filepath.Join(dir, id+".sh")
	if _, err := os.Stat(p); err == nil {
		return
	}
	os.WriteFile(p, script, 0o600)
}

func parseDur(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "lab: "+format+"\n", a...)
	return 2
}
