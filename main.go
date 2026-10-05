// lab: run commands as root on homelab nodes over SSH, with agent-friendly
// (compact, capped, color-free) output.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
  lab put <file|-> <node>:<path>   upload (node/ctr:path works too)
  lab get <node>:<path> [file]     download (default: ./basename)

flags (before target): -t <dur> timeout (default 2m, 0=none)  -f full output
cmd is run via sh -c, so pipes/quotes work: lab pi5 'journalctl -u x -n 50 | grep -i err'
stdin is forwarded: lab pi5 'cat > /etc/foo.conf' <<'EOF' ... EOF
output is capped to first 40 + last 160 lines; non-zero exit prints [exit N].
config: $LAB_CONFIG | ~/.config/lab/nodes | /etc/lab/nodes  (name target description)
`

// Remote environment that keeps output plain and non-interactive.
const envPrefix = "export TERM=dumb NO_COLOR=1 PAGER=cat SYSTEMD_PAGER= SYSTEMD_COLORS=0 DEBIAN_FRONTEND=noninteractive; "

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

type node struct {
	name, user, host, port, desc string
	local                        bool
}

type target struct {
	node      node
	container string
}

type opts struct {
	timeout time.Duration
	full    bool
}

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
		return put(nodes, args[1], args[2], o)
	case "get":
		if len(args) < 2 || len(args) > 3 {
			return fail("usage: lab get <node>:<path> [file]")
		}
		return get(nodes, args[1:], o)
	}

	targets, err := resolve(nodes, args[0])
	if err != nil {
		return fail("%v", err)
	}
	cmd := strings.Join(args[1:], " ")
	if cmd == "" {
		cmd = infoScript
	}
	if len(targets) == 1 {
		return single(targets[0], cmd, o)
	}
	return multi(targets, cmd, o)
}

// ---------- config ----------

func loadConfig() ([]node, error) {
	var paths []string
	if p := os.Getenv("LAB_CONFIG"); p != "" {
		paths = []string{p}
	} else {
		home, _ := os.UserHomeDir()
		paths = []string{filepath.Join(home, ".config/lab/nodes"), "/etc/lab/nodes"}
	}
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
		if n.name == "all" || n.name == "ls" || strings.ContainsAny(n.name, "/,:") {
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

// ---------- execution ----------

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteCommand builds the root shell command for a target. withStdin adds
// docker exec -i so stdin reaches the container.
func remoteCommand(t target, cmd string, timeout time.Duration, withStdin bool) string {
	inner := envPrefix + cmd
	if t.container != "" {
		flag := ""
		if withStdin {
			flag = "-i "
		}
		inner = "docker exec " + flag + shQuote(t.container) + " sh -c " + shQuote(inner)
	}
	c := "sh -c " + shQuote(inner)
	if timeout > 0 {
		c = fmt.Sprintf("timeout -k 5 %d %s", int(timeout.Seconds()), c)
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

func exec1(t target, cmd string, o opts, stdin io.Reader) result {
	ctx := context.Background()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout+15*time.Second)
		defer cancel()
	}
	c := command(ctx, t.node, remoteCommand(t, cmd, o.timeout, stdin != nil), stdin != nil)
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
	logHistory(t, cmd, res)
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

func single(t target, cmd string, o opts) int {
	var in io.Reader
	if stdinIsData() {
		in = os.Stdin
	}
	r := exec1(t, cmd, o, in)
	fmt.Print(r.out)
	if s := status(r, o, t); s != "" {
		fmt.Println(s)
	}
	return r.code
}

func multi(ts []target, cmd string, o opts) int {
	res := make([]result, len(ts))
	var wg sync.WaitGroup
	for i, t := range ts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i] = exec1(t, cmd, o, nil)
		}()
	}
	wg.Wait()
	rc := 0
	for i, t := range ts {
		h := "== " + t.node.name
		if t.container != "" {
			h += "/" + t.container
		}
		if s := status(res[i], o, t); s != "" {
			h += " " + s
			rc = 1
		}
		fmt.Println(h + " ==")
		fmt.Print(res[i].out)
	}
	return rc
}

func list(nodes []node) int {
	o := opts{timeout: 8 * time.Second}
	res := make([]result, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i] = exec1(target{node: n}, probeScript, o, nil)
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

func put(nodes []node, src, dst string, o opts) int {
	t, path, err := splitRemote(nodes, dst)
	if err != nil {
		return fail("%v", err)
	}
	var in io.Reader = os.Stdin
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return fail("%v", err)
		}
		defer f.Close()
		in = f
	}
	cr := &countReader{r: in}
	q := shQuote(path)
	r := exec1(t, fmt.Sprintf(`mkdir -p "$(dirname %s)" && cat > %s`, q, q), o, cr)
	fmt.Print(r.out)
	if r.code != 0 {
		fmt.Println(status(r, o, t))
		return r.code
	}
	fmt.Printf("wrote %d bytes to %s\n", cr.n, dst)
	return 0
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
	ctx := context.Background()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout+15*time.Second)
		defer cancel()
	}
	var out, errb bytes.Buffer
	c := command(ctx, t.node, remoteCommand(t, "cat "+shQuote(path), o.timeout, false), false)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return fail("%s%v", errb.String(), err)
	}
	if err := os.WriteFile(dst, out.Bytes(), 0o644); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("saved %d bytes to %s\n", out.Len(), dst)
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

func logHistory(t target, cmd string, r result) {
	if cmd == probeScript {
		return
	}
	if cmd == infoScript {
		cmd = "<info>"
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".local/state/lab")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "history.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	name := t.node.name
	if t.container != "" {
		name += "/" + t.container
	}
	fmt.Fprintf(f, "%s %s exit=%d dur=%s %s\n", time.Now().Format(time.RFC3339), name, r.code,
		r.dur.Round(time.Millisecond), strings.ReplaceAll(cmd, "\n", "\\n"))
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
