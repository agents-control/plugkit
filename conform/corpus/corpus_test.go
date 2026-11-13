package corpus_test

import (
	"testing"

	"github.com/agents-control/plugkit/conform"
	"github.com/agents-control/plugkit/conform/corpus"
	"github.com/agents-control/plugkit/host"
	"github.com/agents-control/plugkit/outbox/sinktest"
)

func TestRunnerRefusesEveryBrokenFixture(t *testing.T) {
	fixtures, stop, err := corpus.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if err := conform.Verify(t.Context(), &host.Host{}, fixtures, sinktest.Suite()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCatchesAWrongVerdict(t *testing.T) {
	fixtures, stop, err := corpus.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	for i := range fixtures {
		if fixtures[i].Want == "" {
			fixtures[i].Want = "handshake"
		}
	}
	if err := conform.Verify(t.Context(), &host.Host{}, fixtures[:1], sinktest.Suite()); err == nil {
		t.Fatal("a passing plugin expected to fail must be reported")
	}
	if err := conform.Verify(t.Context(), &host.Host{}, fixtures[:1]); err == nil {
		t.Fatal("a port without a suite must fail the run")
	}
}
