package adapters

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agents-control/plugkit/discovery"
	"github.com/agents-control/plugkit/host"
)

// Main runs "adapters check --file f" or "adapters plan --file f --against url" for the engine described by spec.
// It returns 0 when the file is valid (check) or the plan was printed (plan), 1 on findings or errors, 2 on misuse.
func Main(spec Spec, args []string, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "check" && args[0] != "plan") {
		fmt.Fprintln(stderr, "usage: adapters check --file f | adapters plan --file f --against url [--token env:NAME] [--header Name=env:NAME] [--json]")
		return 2
	}
	fs := flag.NewFlagSet("adapters "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "adapters.yaml; empty reads the env form only")
	against := fs.String("against", "", "base URL of the running engine")
	asJSON := fs.Bool("json", false, "print the plan as JSON")
	token := fs.String("token", "", "bearer token reference for --against, such as env:NAME")
	var headers []string
	fs.Func("header", "Name=reference header for --against, repeatable", func(v string) error {
		headers = append(headers, v)
		return nil
	})
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	var data []byte
	if *file != "" {
		var err error
		if data, err = os.ReadFile(*file); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	f, err := Load(spec, data, environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	findings := Check(spec, f)
	for _, x := range findings {
		fmt.Fprintln(stdout, x)
	}
	if findings.Err() != nil {
		return 1
	}
	if args[0] == "check" {
		fmt.Fprintln(stdout, "adapters ok")
		return 0
	}
	if *against == "" {
		fmt.Fprintln(stderr, "plan needs --against")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts, err := fetchAuth(ctx, *token, headers)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	current, err := discovery.Fetch(ctx, http.DefaultClient, *against, opts...)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	plan := Diff(spec, f, current)
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(plan)
		return 0
	}
	for _, c := range plan.Changes {
		fmt.Fprintf(stdout, "~ %s: %v -> %v", c.Port, c.From, c.To)
		for _, w := range c.Weakening {
			fmt.Fprintf(stdout, " [weakening: %s]", w)
		}
		if c.DataMigrationRequired {
			fmt.Fprint(stdout, " [data_migration_required]")
		}
		fmt.Fprintln(stdout)
	}
	fmt.Fprintf(stdout, "%d changes, weakening=%t, data_migration_required=%t\n", len(plan.Changes), plan.Weakening(), plan.DataMigrationRequired())
	return 0
}

func fetchAuth(ctx context.Context, token string, headers []string) ([]discovery.Option, error) {
	var opts []discovery.Option
	resolve := func(ref string) (string, error) {
		if !host.IsRef(ref) {
			return "", fmt.Errorf("%q must be a reference such as env:NAME, never a value", ref)
		}
		return host.EnvSecrets{}.Resolve(ctx, ref)
	}
	if token != "" {
		v, err := resolve(token)
		if err != nil {
			return nil, fmt.Errorf("--token: %w", err)
		}
		opts = append(opts, discovery.WithBearer(v))
	}
	for _, h := range headers {
		name, ref, ok := strings.Cut(h, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--header %q must be Name=reference", h)
		}
		v, err := resolve(ref)
		if err != nil {
			return nil, fmt.Errorf("--header %s: %w", name, err)
		}
		opts = append(opts, discovery.WithHeader(name, v))
	}
	return opts, nil
}
