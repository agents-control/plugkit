package adapters

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/agents-control/plugkit/discovery"
)

// Main runs "adapters check --file f" or "adapters plan --file f --against url" for the engine described by spec.
// It returns 0 when the file is valid (check) or the plan was printed (plan), 1 on findings or errors, 2 on misuse.
func Main(spec Spec, args []string, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "check" && args[0] != "plan") {
		fmt.Fprintln(stderr, "usage: adapters check --file f | adapters plan --file f --against url [--json]")
		return 2
	}
	fs := flag.NewFlagSet("adapters "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "adapters.yaml; empty reads the env form only")
	against := fs.String("against", "", "base URL of the running engine")
	asJSON := fs.Bool("json", false, "print the plan as JSON")
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
	current, err := discovery.Fetch(ctx, http.DefaultClient, *against)
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
