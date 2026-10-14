package pipeline

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func textLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

func TestLineLoggerSplitsAcrossWrites(t *testing.T) {
	var buf bytes.Buffer
	l := &lineLogger{log: textLogger(&buf), stream: "stdout"}
	l.Write([]byte("first li"))
	l.Write([]byte("ne\nsecond line\nthi"))
	l.Write([]byte("rd\r progress 50%\r progress 100%\n\n"))
	l.Write([]byte("tail without newline"))
	l.flush()
	out := buf.String()
	for _, want := range []string{`msg="first line"`, `msg="second line"`, `msg=" progress 100%"`, `msg="tail without newline"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "third") || strings.Contains(out, "50%") {
		t.Errorf("carriage-return handling wrong:\n%s", out)
	}
	if n := strings.Count(out, "\n"); n != 4 {
		t.Errorf("want 4 log lines, got %d:\n%s", n, out)
	}
}

func TestCmdStringMasksPassword(t *testing.T) {
	c := Cmd{Name: "docker", Args: []string{"run", "-user", "nominatim", "-password", "s3cret", "-languages", "ru"}}
	got := c.String()
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "-password ***") || !strings.Contains(got, "-languages ru") {
		t.Errorf("String() = %q", got)
	}
}

func TestExecRunnerRunAndOutput(t *testing.T) {
	var buf bytes.Buffer
	r := ExecRunner{}
	if err := r.Run(context.Background(), textLogger(&buf), Cmd{Name: "sh", Args: []string{"-c", "echo hello; echo oops >&2"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "hello") || !strings.Contains(buf.String(), "oops") {
		t.Errorf("output not logged: %s", buf.String())
	}
	out, err := r.Output(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo $FOO"}, Env: map[string]string{"FOO": "bar"}})
	if err != nil || strings.TrimSpace(out) != "bar" {
		t.Errorf("Output = %q, %v", out, err)
	}
	_, err = r.Output(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failure should carry stderr, got %v", err)
	}
}

// testLogger discards output.
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
