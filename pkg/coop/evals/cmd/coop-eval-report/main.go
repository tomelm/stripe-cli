package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop/evals"
)

type stringList []string

func (l *stringList) String() string {
	return strings.Join(*l, ",")
}

func (l *stringList) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

func main() {
	var resultsDirs stringList
	var outputPath string
	var title string
	var fixesPath string
	var portable bool

	flag.Var(&resultsDirs, "results-dir", "eval results directory; may be repeated or comma-separated")
	flag.StringVar(&outputPath, "output", "", "output HTML path")
	flag.StringVar(&title, "title", "Co-op Eval Report", "report title")
	flag.StringVar(&fixesPath, "fixes", "", "optional JSON file describing fixes between runs")
	flag.BoolVar(&portable, "portable", false, "write relative artifact links instead of absolute file:// links")
	flag.Parse()

	resultsDirs = append(resultsDirs, flag.Args()...)
	if len(resultsDirs) == 0 {
		fmt.Fprintln(os.Stderr, "at least one --results-dir or positional results directory is required")
		os.Exit(2)
	}

	if err := evals.WriteHTMLReport(evals.ReportOptions{
		ResultsDirs: resultsDirs,
		OutputPath:  outputPath,
		Title:       title,
		FixesPath:   fixesPath,
		Portable:    portable,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
