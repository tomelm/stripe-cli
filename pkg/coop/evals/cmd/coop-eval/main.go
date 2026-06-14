package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop/evals"
)

type caseList []string

func (c *caseList) String() string {
	return strings.Join(*c, ",")
}

func (c *caseList) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*c = append(*c, part)
		}
	}
	return nil
}

func main() {
	var cases caseList
	var opts evals.Options
	var timeout time.Duration

	flag.StringVar(&opts.RepoRoot, "repo-root", "", "repository root")
	flag.StringVar(&opts.CasesDir, "cases-dir", "", "eval case directory")
	flag.StringVar(&opts.FixturesDir, "fixtures-dir", "", "fixture project directory")
	flag.StringVar(&opts.ResultsDir, "results-dir", "", "directory for eval artifacts")
	flag.StringVar(&opts.StripeBin, "stripe-bin", "", "candidate stripe binary; builds one if omitted")
	flag.StringVar(&opts.Agent, "agent", "", "agent adapter: debug or command")
	flag.StringVar(&opts.AgentCommand, "agent-command", "", "shell command for --agent=command; use $COOP_EVAL_PROMPT_FILE and stripe from PATH")
	flag.StringVar(&opts.Suite, "suite", "", "case suite: default, all, complex, or tag:<tag>")
	flag.IntVar(&opts.MinSteps, "min-steps", 0, "only run cases whose blueprint has at least this many steps")
	flag.BoolVar(&opts.DisableAgentSandbox, "disable-agent-sandbox", false, "do not wrap command agents with the host-browser sandbox")
	flag.Var(&cases, "case", "case id to run; may be repeated or comma-separated")
	flag.DurationVar(&timeout, "timeout", 5*time.Minute, "per-case timeout")
	flag.BoolVar(&opts.KeepWork, "keep-workspace", true, "retain fixture workspaces in artifacts")
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			opts.TimeoutSet = true
		}
	})
	opts.CaseIDs = cases
	opts.Timeout = timeout

	suite, err := evals.NewRunner(opts).Run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("results: %s\n", suite.ResultsDir)
	if !suite.Passed {
		os.Exit(1)
	}
}
