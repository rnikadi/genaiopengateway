package mcpgw

import (
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync"
)

// maxStderrLine bounds one logged line of a stdio server's stderr.
const maxStderrLine = 1024

// command builds the process of a hosted stdio server. It does not
// inherit the gateway's environment, which holds the gateway's own
// secrets: it sees PATH and HOME, and the server's env.
func (u *upstream) command() *exec.Cmd {
	cmd := exec.Command(u.cfg.Command[0], u.cfg.Command[1:]...)
	cmd.Dir = u.cfg.Dir
	env := []string{"PATH=" + os.Getenv("PATH")}
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	}
	names := make([]string, 0, len(u.cfg.Env))
	for k := range u.cfg.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		env = append(env, k+"="+u.cfg.Env[k].Value())
	}
	cmd.Env = env
	cmd.Stderr = &stderrLogger{name: u.name}
	return cmd
}

// stderrLogger logs each line a stdio server writes to stderr, so its
// errors reach the gateway's log, cut to maxStderrLine bytes.
type stderrLogger struct {
	name string
	mu   sync.Mutex
	buf  []byte
}

func (l *stderrLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			l.flush()
			continue
		}
		if len(l.buf) < maxStderrLine {
			l.buf = append(l.buf, b)
		}
	}
	return len(p), nil
}

func (l *stderrLogger) flush() {
	if len(l.buf) > 0 {
		slog.Warn("mcp server stderr", "server", l.name, "line", string(l.buf))
	}
	l.buf = l.buf[:0]
}
