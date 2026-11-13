package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agents-control/plugkit/conform"
	"github.com/agents-control/plugkit/conform/corpus"
	"github.com/agents-control/plugkit/outbox/sinktest"
)

var cli = conform.CLI{Suites: []conform.Suite{sinktest.Suite()}, Corpus: corpus.Start}

func startPython(t *testing.T, args ...string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for the example plugin test")
	}
	cmd := exec.Command(python, append([]string{"-I", filepath.Join("..", "..", "examples", "python-sink", "plugin.py")}, args...)...)
	out, _ := cmd.StdoutPipe()
	cmd.Env = append(os.Environ(), "PLUGKIT_EXAMPLE_TOKEN=s3cret")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	port, ok := strings.CutPrefix(strings.TrimSpace(line), "LISTENING ")
	if err != nil || !ok {
		t.Fatalf("plugin did not start: %q %v", line, err)
	}
	return "http://127.0.0.1:" + port
}

func TestPythonSinkPassesConformance(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	endpoint := startPython(t, "--token-env", "PLUGKIT_EXAMPLE_TOKEN", "--out", events)
	report := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("PLUGKIT_EXAMPLE_TOKEN", "s3cret")
	var out, errOut bytes.Buffer
	args := []string{"-endpoint", endpoint, "-protocol", "http", "-insecure", "-credential", "env:PLUGKIT_EXAMPLE_TOKEN", "-report", report}
	if code := cli.Main(args, &out, &errOut); code != 0 {
		t.Fatalf("conform = %d\n%s%s", code, out.String(), errOut.String())
	}
	var r conform.Report
	raw, _ := os.ReadFile(report)
	if err := json.Unmarshal(raw, &r); err != nil || !r.Passed || r.Plugin != "python-sink" || r.Digest() == "" {
		t.Fatalf("report = %+v %v", r, err)
	}
	lines, _ := os.ReadFile(events)
	if n := bytes.Count(lines, []byte("\n")); n != 3 {
		t.Fatalf("delivered %d events, want 3 deduplicated", n)
	}
	t.Setenv("PLUGKIT_EXAMPLE_TOKEN", "wrong")
	if code := cli.Main(args[:len(args)-2], &out, &errOut); code != 1 {
		t.Fatalf("a wrong token must fail the run, got %d", code)
	}
}

func TestCorpusCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"corpus"}, &out, &errOut); code != 0 {
		t.Fatalf("corpus = %d: %s", code, errOut.String())
	}
	if code := cli.Main([]string{"-protocol", "ws", "-endpoint", "x"}, &out, &errOut); code != 1 {
		t.Fatalf("an invalid config is a failing handshake, got %d", code)
	}
}

func TestCorpusHonoursTimeout(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"corpus", "-timeout", "1ns"}, &out, &errOut); code != 1 {
		t.Fatalf("an expired deadline must fail the corpus, got %d: %s", code, errOut.String())
	}
}

func TestCorpusRejectsEndpointFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"corpus", "-endpoint", "x:1"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "-endpoint") {
		t.Fatalf("code = %d: %s", code, errOut.String())
	}
}
