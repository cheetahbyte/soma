package claudecode

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cheetahbyte/soma"
	"github.com/cheetahbyte/soma/config"
	"github.com/cheetahbyte/soma/toolserver"
)

const (
	serverName   = "axon"
	secretEnv    = "SOMA_TOOL_SECRET"
	callTimeout  = 600000
	stderrKeep   = 4096
	stderrReport = 2000
	stopGrace    = 2 * time.Second
)

type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	lines  chan []byte
	exited chan struct{}
	waited chan struct{}
	quit   chan struct{}
	once   sync.Once

	tailMu sync.Mutex
	tail   []byte

	server *toolserver.ToolServer
	box    *mailbox

	oneShot   bool
	resumed   bool
	model     string
	effort    string
	tools     []string
	sessionID string
	cost      float64 // Claude Code's running cost already handed out
	estimated float64 // estimates handed out since the last result line
	base      opaque  // the reply a resumed process continues from
	total     float64 // Claude Code's running cost at the last result line
	allTokens int     // Claude Code's running token count at the last result line
	tokens    int     // tokens of this process's own replies
	settled   bool    // a result line has fixed where pr.cost starts
	price     func(id string) config.ModelInfo
	overage   bool
	produced  bool
	sawRate   bool
}

type tailWriter struct{ p *proc }

func (w tailWriter) Write(b []byte) (int, error) {
	w.p.tailMu.Lock()
	defer w.p.tailMu.Unlock()
	w.p.tail = append(w.p.tail, b...)
	if len(w.p.tail) > stderrKeep {
		w.p.tail = w.p.tail[len(w.p.tail)-stderrKeep:]
	}
	return len(b), nil
}

func (pr *proc) stderrTail() string {
	pr.tailMu.Lock()
	defer pr.tailMu.Unlock()
	t := pr.tail
	if len(t) > stderrReport {
		t = t[len(t)-stderrReport:]
	}
	return string(t)
}

func newSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func mcpConfig(url string) (string, error) {
	b, err := json.Marshal(map[string]any{"mcpServers": map[string]any{serverName: map[string]any{
		"type":    "http",
		"url":     url,
		"headers": map[string]string{"Authorization": "Bearer ${" + secretEnv + "}"},
		"timeout": callTimeout,
	}}})
	return string(b), err
}

func servedTools(specs []soma.ToolSpec) []toolserver.ServedTool {
	out := make([]toolserver.ServedTool, len(specs))
	for i, s := range specs {
		out[i] = toolserver.ServedTool{Name: s.Name, Description: s.Description, Schema: s.Schema, ReadOnly: readOnly[s.Name]}
	}
	return out
}

func toolNames(specs []soma.ToolSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// spawn starts claude for req in dir, or in the current directory when dir
// is empty. Without oneShot it also starts the tool server the process is
// configured with; the secret is passed in the environment only.
func spawn(command, dir string, req soma.Request, resume string, oneShot bool, price func(id string) config.ModelInfo) (*proc, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	pr := &proc{
		price:   price,
		oneShot: oneShot,
		resumed: resume != "",
		model:   req.Model,
		effort:  effort(req.Thinking),
		tools:   toolNames(req.Tools),
		box:     newMailbox(),
		lines:   make(chan []byte, 64),
		exited:  make(chan struct{}),
		waited:  make(chan struct{}),
		quit:    make(chan struct{}),
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--model", req.Model}
	if pr.effort != "" {
		args = append(args, "--effort", pr.effort)
	}
	args = append(args, "--system-prompt", req.System, "--tools", "", "--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands", "--system-prompt-snapshot", "off")
	env := append(childEnv(), "DISABLE_AUTO_COMPACT=1")
	if oneShot {
		args = append(args, "--no-session-persistence")
	} else {
		secret, err := newSecret()
		if err != nil {
			return nil, err
		}
		pr.server, err = toolserver.Serve(servedTools(req.Tools), secret, pr.handle)
		if err != nil {
			return nil, fmt.Errorf("starting the tool server: %w", err)
		}
		cfg, err := mcpConfig(pr.server.URL())
		if err != nil {
			pr.server.Close()
			return nil, err
		}
		args = append(args, "--mcp-config", cfg, "--allowedTools", mcpPrefix+"*")
		if resume != "" {
			args = append(args, "--resume", resume)
		}
		env = append(env, secretEnv+"="+secret)
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = tailWriter{pr}
	pr.cmd = cmd
	fail := func(err error) (*proc, error) {
		if pr.server != nil {
			pr.server.Close()
		}
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	pr.stdin = stdin
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("starting %s: %w", command, err))
	}
	go pr.read(stdout)
	go func() {
		<-pr.exited
		cmd.Wait()
		close(pr.waited)
	}()
	return pr, nil
}

// read passes stdout lines on until the process ends. After stop it keeps
// draining so that the process never blocks on a full pipe.
func (pr *proc) read(r io.Reader) {
	defer close(pr.exited)
	defer close(pr.lines)
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			select {
			case pr.lines <- line:
			case <-pr.quit:
			}
		}
		if err != nil {
			return
		}
	}
}

func (pr *proc) alive() bool {
	select {
	case <-pr.exited:
		return false
	default:
		return true
	}
}

func (pr *proc) write(b []byte) error {
	pr.wmu.Lock()
	defer pr.wmu.Unlock()
	_, err := pr.stdin.Write(b)
	return err
}

func (pr *proc) signal(sig syscall.Signal) {
	if pr.cmd.Process != nil {
		syscall.Kill(-pr.cmd.Process.Pid, sig)
	}
}

// interrupt asks claude to abort what it is doing, as Esc does.
func (pr *proc) interrupt() { pr.signal(syscall.SIGINT) }

// stop ends the process and its tool server: stdin closed, SIGTERM to the
// group after two seconds, SIGKILL after two more. Calls still held get
// SIGINT first, so claude does not wait for answers that never come.
func (pr *proc) stop() {
	pr.once.Do(func() {
		close(pr.quit)
		if pr.box.outstanding() > 0 {
			pr.interrupt()
		}
		pr.stdin.Close()
		if !waitFor(pr.waited, stopGrace) {
			pr.signal(syscall.SIGTERM)
			if !waitFor(pr.waited, stopGrace) {
				pr.signal(syscall.SIGKILL)
				<-pr.waited
			}
		}
		if pr.server != nil {
			pr.server.Close()
		}
	})
}

func waitFor(ch <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// StripEnv prevents API-key billing and nested Claude Code detection.
var StripEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}

func childEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(StripEnv, name) {
			out = append(out, kv)
		}
	}
	return out
}
