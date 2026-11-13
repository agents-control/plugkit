// Command conform checks any acplug/1 plugin endpoint against the port suites plugkit ships, without Go on the plugin side.
package main

import (
	"os"

	"github.com/agents-control/plugkit/conform"
	"github.com/agents-control/plugkit/conform/corpus"
	"github.com/agents-control/plugkit/outbox/sinktest"
)

func main() {
	cli := conform.CLI{Suites: []conform.Suite{sinktest.Suite()}, Corpus: corpus.Start}
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
