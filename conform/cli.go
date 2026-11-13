package conform

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/agents-control/plugkit/host"
)

// CLI is the conform command an engine binary embeds; Corpus, when set, enables the "corpus" self-test.
type CLI struct {
	Host   host.Host
	Suites []Suite
	Corpus func() ([]Fixture, func(), error)
}

// Main runs "conform [flags]" against one endpoint, or "conform corpus"; it returns 0 on pass, 1 on failure, 2 on misuse.
func (cli CLI) Main(args []string, stdout, stderr io.Writer) int {
	h := cli.Host
	if h.Secrets == nil {
		h.Secrets = host.EnvSecrets{}
	}
	if len(args) > 0 && args[0] == "corpus" {
		return cli.corpus(&h, stdout, stderr)
	}
	fs := flag.NewFlagSet("conform", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg host.Config
	var ports, report, settings string
	var timeout time.Duration
	fs.StringVar(&cfg.Endpoint, "endpoint", "", "plugin endpoint: host:port for grpc, http(s)://host:port for http")
	fs.StringVar(&cfg.Protocol, "protocol", host.ProtocolGRPC, "grpc or http")
	fs.StringVar(&cfg.Name, "name", "under-test", "name used in errors")
	fs.BoolVar(&cfg.TLS.Insecure, "insecure", false, "talk plaintext")
	fs.StringVar(&cfg.TLS.CA, "ca", "", "CA bundle that signs the plugin certificate")
	fs.StringVar(&cfg.TLS.Cert, "cert", "", "client certificate for mTLS")
	fs.StringVar(&cfg.TLS.Key, "key", "", "client key for mTLS")
	fs.StringVar(&cfg.TLS.ServerName, "server-name", "", "TLS server name override")
	fs.StringVar(&cfg.CredentialRef, "credential", "", "bearer token reference: env:NAME or file:/path")
	fs.StringVar(&ports, "ports", "", "comma-separated ports to check; empty checks every declared port")
	fs.StringVar(&settings, "config", "", "JSON file mapping each port to its config")
	fs.DurationVar(&timeout, "timeout", 5*time.Minute, "deadline of the whole run, interrupt cancels it earlier")
	fs.StringVar(&report, "report", "", "write the JSON report to this path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if ports != "" {
		cfg.Ports = strings.Split(ports, ",")
	}
	if settings != "" {
		raw, err := os.ReadFile(settings)
		if err == nil {
			err = json.Unmarshal(raw, &cfg.Settings)
		}
		if err != nil {
			fmt.Fprintf(stderr, "config: %v\n", err)
			return 2
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r := Run(ctx, &h, cfg, cli.Suites...)
	for _, c := range r.Cases {
		verdict, name, detail := "PASS", c.Case, c.Error
		switch {
		case c.Skipped:
			verdict, detail = "SKIP", c.Reason
		case !c.Passed:
			verdict = "FAIL"
		}
		if c.Port != "" {
			name = c.Port + "/" + c.Case
		}
		fmt.Fprintf(stdout, "%s %s (%.1f ms) %s\n", verdict, name, c.Millis, detail)
	}
	fmt.Fprintf(stdout, "report %s passed=%t\n", r.Digest(), r.Passed)
	if report != "" {
		raw, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(report, append(raw, '\n'), 0o644); err != nil {
			fmt.Fprintf(stderr, "report: %v\n", err)
			return 2
		}
	}
	if !r.Passed {
		return 1
	}
	return 0
}

func (cli CLI) corpus(h *host.Host, stdout, stderr io.Writer) int {
	if cli.Corpus == nil {
		fmt.Fprintln(stderr, "this binary ships no corpus")
		return 2
	}
	fixtures, stop, err := cli.Corpus()
	if err != nil {
		fmt.Fprintf(stderr, "corpus: %v\n", err)
		return 2
	}
	defer stop()
	if err := Verify(context.Background(), h, fixtures, cli.Suites...); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "corpus ok: %d fixtures, every broken one refused\n", len(fixtures))
	return 0
}
