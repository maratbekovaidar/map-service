package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Cmd describes an external command.
type Cmd struct {
	Name string
	Args []string
	Env  map[string]string // added to the inherited environment
}

// String renders the command for logs. Arguments that follow a password flag
// are masked.
func (c Cmd) String() string {
	parts := make([]string, 0, len(c.Args)+1)
	parts = append(parts, c.Name)
	mask := false
	for _, a := range c.Args {
		if mask {
			a, mask = "***", false
		} else if a == "-password" || a == "--password" {
			mask = true
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// Runner executes external commands. The pipeline never calls os/exec
// directly so tests can substitute a recording fake.
type Runner interface {
	// Run executes c, streaming its output to log line by line.
	Run(ctx context.Context, log *slog.Logger, c Cmd) error
	// Output executes c and returns its stdout.
	Output(ctx context.Context, c Cmd) (string, error)
}

// ExecRunner is the production Runner backed by os/exec.
type ExecRunner struct{}

func (ExecRunner) command(ctx context.Context, c Cmd) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = os.Environ()
	for k, v := range c.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// On cancellation: SIGINT first so docker/osmium can clean up, SIGKILL later.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	return cmd
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, log *slog.Logger, c Cmd) error {
	cmd := r.command(ctx, c)
	out := &lineLogger{log: log, stream: "stdout"}
	errw := &lineLogger{log: log, stream: "stderr"}
	cmd.Stdout, cmd.Stderr = out, errw
	log.Info("exec", "cmd", c.String())
	err := cmd.Run()
	out.flush()
	errw.flush()
	if err != nil {
		return fmt.Errorf("%s: %w", c.Name, err)
	}
	return nil
}

// Output implements Runner.
func (r ExecRunner) Output(ctx context.Context, c Cmd) (string, error) {
	cmd := r.command(ctx, c)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		var ee *exec.ExitError
		if errors.As(err, &ee) && msg != "" {
			return stdout.String(), fmt.Errorf("%s: %w: %s", c.Name, err, lastLines(msg, 5))
		}
		return stdout.String(), fmt.Errorf("%s: %w", c.Name, err)
	}
	return stdout.String(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// lineLogger is an io.Writer that logs complete lines.
type lineLogger struct {
	log    *slog.Logger
	stream string
	mu     sync.Mutex
	buf    []byte
}

var _ io.Writer = (*lineLogger)(nil)

// maxLine bounds memory if a tool prints a huge line without a newline.
const maxLine = 1 << 20

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.emit(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > maxLine {
		l.emit(string(l.buf))
		l.buf = nil
	}
	return len(p), nil
}

func (l *lineLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.emit(string(l.buf))
		l.buf = nil
	}
}

func (l *lineLogger) emit(line string) {
	// Progress bars rewrite the line with \r; keep only the final state.
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	if strings.TrimSpace(line) == "" {
		return
	}
	l.log.Info(line, "stream", l.stream)
}
