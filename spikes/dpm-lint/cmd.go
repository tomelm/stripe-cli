package main

// cmd.go — the command surface, following the CLI's cobra conventions.
// Every command supports --json (machine contract on stdout, logs on stderr)
// and the exit-code contract documented in reports.go and `dpm guide`.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var (
	flagJSON    bool
	flagProfile string
	flagYes     bool
)

func exitWith(code int) { os.Exit(code) }

func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(err error) {
	if flagJSON {
		emitJSON(map[string]any{"error": err.Error()})
	} else {
		fmt.Fprintln(os.Stderr, failLine(err.Error()))
	}
	exitWith(2)
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "dpm",
		Short: "Dynamic Payment Methods migration toolkit (demo)",
		Long: `Scan for hardcoded payment_method_types across 7 languages, judge each
finding against live account facts, preview/apply span-verified removals,
and prove the migration's runtime behavior with local experiments.

Humans: start with ` + "`dpm demo`" + `. Agents: start with ` + "`dpm guide`" + `.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "machine-readable output on stdout, logs on stderr")
	root.PersistentFlags().StringVar(&flagProfile, "profile", "default", "Stripe CLI config profile for account access")
	root.PersistentFlags().BoolVarP(&flagYes, "yes", "y", false, "assume yes for confirmations (non-interactive)")

	root.AddCommand(newScanCmd(), newDoctorCmd(), newFixCmd(), newDrillCmd(),
		newExperimentCmd(), newDemoCmd(), newGuideCmd(), newCleanupCmd(), newDumpCmd())
	return root
}

// ---------- scan ----------

func newScanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scan [dir]",
		Short: "Find hardcoded payment_method_types (exit 1 if findings)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := argOr(args, ".")
			findings, scanned, parsed := scan(root, dpmRule)
			sortFindings(findings)
			rep := ScanReport{Command: "scan", Findings: findings,
				Stats: ScanStats{FilesScanned: scanned, FilesParsed: parsed, Skipped: scanned - parsed}}
			if flagJSON {
				emitJSON(rep)
			} else {
				renderScan(rep)
			}
			if len(findings) > 0 {
				exitWith(1)
			}
			return nil
		},
	}
}

func renderScan(rep ScanReport) {
	if len(rep.Findings) == 0 {
		fmt.Println(okLine(fmt.Sprintf("no hardcoded payment_method_types — %d files scanned, %d parsed",
			rep.Stats.FilesScanned, rep.Stats.FilesParsed)))
		return
	}
	for _, f := range rep.Findings {
		fmt.Printf("%s %s\n", warnStyle.Render("●"), titleStyle.Render(fmt.Sprintf("%s:%d:%d", f.File, f.Line, f.Col)))
		fmt.Println(kv("value", f.Value))
		fmt.Println(kv("resolved via", f.Via))
		fmt.Println(kv("in", f.Anchor))
	}
	fmt.Printf("\n%s\n", mutedStyle.Render(fmt.Sprintf("%d finding(s) — %d files scanned, %d parsed (%d skipped by prefilter) — %s",
		len(rep.Findings), rep.Stats.FilesScanned, rep.Stats.FilesParsed, rep.Stats.Skipped, dpmRule.Docs)))
}

// ---------- doctor ----------

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [dir]",
		Short: "Scan plus account-aware verdicts (read-only API calls)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := argOr(args, ".")
			var rep *DoctorReport
			err := withSpinnerUnlessJSON("Scanning and fetching account facts", func() error {
				findings, scanned, parsed := scan(root, dpmRule)
				sortFindings(findings)
				var derr error
				rep, derr = buildDoctorReport(findings, flagProfile)
				if rep != nil {
					rep.Stats = ScanStats{FilesScanned: scanned, FilesParsed: parsed, Skipped: scanned - parsed}
				}
				return derr
			})
			if err != nil {
				fail(err)
			}
			if flagJSON {
				emitJSON(rep)
			} else {
				renderDoctor(rep)
			}
			if len(rep.Findings) > 0 {
				exitWith(1)
			}
			return nil
		},
	}
}

func renderDoctor(r *DoctorReport) {
	a := r.Account
	name := a.ID
	if a.Name != "" {
		name += " (" + a.Name + ")"
	}
	fmt.Println(titleStyle.Render("Account ") + name + mutedStyle.Render("  test mode"))

	var vs []string
	for v, n := range a.EventVersions {
		vs = append(vs, fmt.Sprintf("%s ×%d", v, n))
	}
	sort.Strings(vs)
	fmt.Println(kv("event API versions", strings.Join(vs, ", ")))
	verLine := fmt.Sprintf("all at/after %s", a.Cutoff)
	if !a.VersionsOK {
		verLine = failStyle.Render(fmt.Sprintf("traffic predates %s", a.Cutoff))
	}
	fmt.Println(kv("DPM version cutoff", verLine))
	cfg := fmt.Sprintf("%d configuration(s), %d methods on / %d off", a.Configs, a.MethodsOn, a.MethodsOff)
	if a.ActiveConfig != "" {
		cfg += mutedStyle.Render(" — active: " + a.ActiveConfig)
	}
	fmt.Println(kv("dashboard methods", cfg))

	if len(r.Findings) == 0 {
		fmt.Println("\n" + okLine("no hardcoded payment_method_types found — nothing to migrate"))
		return
	}
	fmt.Printf("\n%s\n", titleStyle.Render(fmt.Sprintf("Verdicts (%d findings)", len(r.Findings))))
	for _, f := range r.Findings {
		glyph := warnLine
		switch f.Class {
		case "CANDIDATE":
			glyph = okLine
		case "BLOCKED":
			glyph = failLine
		}
		fmt.Println(glyph(fmt.Sprintf("%s:%d  %s %s", f.File, f.Line, mutedStyle.Render("["+f.Intent+"]"), f.Value)))
		fmt.Println(kv("", f.Verdict))
	}
	var sum []string
	for class, n := range r.Summary {
		sum = append(sum, fmt.Sprintf("%s ×%d", class, n))
	}
	sort.Strings(sum)
	fmt.Println("\n" + mutedStyle.Render("  "+strings.Join(sum, "  ")))
}

// ---------- fix ----------

func newFixCmd() *cobra.Command {
	var apply bool
	c := &cobra.Command{
		Use:   "fix [dir]",
		Short: "Preview span-verified removals (dry-run); --apply writes reparse-clean files",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := argOr(args, ".")
			if apply && !confirm("Apply removals in place? (only reparse-clean files are written)", flagYes) {
				fmt.Println(infoLine("aborted; nothing written"))
				exitWith(1)
			}
			rep, err := fixRun(root, dpmRule, apply)
			if err != nil {
				fail(err)
			}
			if flagJSON {
				emitJSON(rep)
			} else {
				renderFix(rep)
			}
			if !rep.AllClean {
				exitWith(1)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "write changes (dry-run without this flag)")
	return c
}

func renderFix(r *FixReport) {
	mode := "dry-run (nothing written)"
	if r.Applied {
		mode = "APPLIED"
	}
	fmt.Println(titleStyle.Render("Removals — " + mode))
	for _, f := range r.Files {
		labels := map[string]int{}
		for _, e := range f.Edits {
			labels[e.Label]++
		}
		var ls []string
		for l, n := range labels {
			ls = append(ls, fmt.Sprintf("%s×%d", l, n))
		}
		sort.Strings(ls)
		line := fmt.Sprintf("%s  −%d bytes  %s", f.Path, f.BytesRemoved, mutedStyle.Render(strings.Join(ls, ", ")))
		switch {
		case f.Reparse == "error":
			fmt.Println(failLine(line + "  reparse ERROR — not written"))
		case f.Written:
			fmt.Println(okLine(line + "  written"))
		default:
			fmt.Println(okLine(line + "  reparse clean"))
		}
	}
	if len(r.Files) == 0 {
		fmt.Println(infoLine("nothing to fix"))
	}
}

// ---------- drill ----------

func newDrillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drill",
		Short: "Verify delayed-notification webhooks end to end (listen + trigger)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var rep *DrillReport
			err := withSpinnerUnlessJSON("Running webhook drill (listen, trigger, verify signatures)", func() error {
				var derr error
				rep, derr = drillRun()
				return derr
			})
			if err != nil {
				fail(err)
			}
			if flagJSON {
				emitJSON(rep)
			} else {
				renderDrill(rep)
			}
			if !rep.Verified {
				exitWith(1)
			}
			return nil
		},
	}
}

func renderDrill(r *DrillReport) {
	fmt.Println(titleStyle.Render("Delayed-notification drill"))
	if r.ListenReady {
		fmt.Println(okLine("stripe listen ready (signing secret captured, not shown)"))
	}
	if r.Triggered != "" {
		fmt.Println(okLine("triggered " + r.Triggered))
	}
	for _, e := range r.Events {
		if e.Signature == "verified" {
			fmt.Println(okLine(fmt.Sprintf("received %-45s signature verified", e.Type)))
		} else {
			fmt.Println(failLine(fmt.Sprintf("received %-45s signature INVALID", e.Type)))
		}
	}
	if r.Verified {
		fmt.Println(okLine(titleStyle.Render("verified: the doc's handler round-trips end to end")))
	} else {
		fmt.Println(warnLine("not fully verified" + mutedStyle.Render(" — "+r.Note)))
	}
}

// ---------- experiment ----------

func newExperimentCmd() *cobra.Command {
	var keep bool
	c := &cobra.Command{
		Use:   "experiment",
		Short: "Prove Dashboard config drives methods (ephemeral test-mode config, auto-cleaned)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm("Create an ephemeral TEST-mode payment-method configuration? (your Default config is never touched)", flagYes) {
				fmt.Println(infoLine("aborted"))
				exitWith(1)
			}
			var rep *ExperimentReport
			err := withSpinnerUnlessJSON("Running config→session experiment", func() error {
				var derr error
				rep, derr = experimentRun(flagProfile, keep)
				return derr
			})
			if err != nil {
				if rep != nil && flagJSON {
					emitJSON(rep)
				}
				fail(err)
			}
			if flagJSON {
				emitJSON(rep)
			} else {
				renderExperiment(rep)
			}
			if !rep.Verified {
				exitWith(1)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&keep, "keep-active", false, "leave the ephemeral config active (clean up with `dpm cleanup <id>`)")
	return c
}

func renderExperiment(r *ExperimentReport) {
	fmt.Println(titleStyle.Render("Config → session experiment") + mutedStyle.Render("  (ephemeral config "+r.ConfigID+")"))
	fmt.Println(kv("session before", strings.Join(r.Before.Methods, ", ")))
	fmt.Println(kv("toggled off", r.Toggled))
	fmt.Println(kv("session after", strings.Join(r.After.Methods, ", ")))
	fmt.Println(kv("diff", fmt.Sprintf("removed=%v added=%v", r.Removed, r.Added)))
	if r.Verified {
		fmt.Println(okLine(titleStyle.Render("verified: the Dashboard config change alone changed what customers see")))
	} else {
		fmt.Println(warnLine("unexpected diff — inspect manually"))
	}
	fmt.Println(kv("cleanup", r.Cleanup))
	if r.Before.URL != "" {
		fmt.Println(kv("hosted page (before)", mutedStyle.Render(r.Before.URL)))
	}
}

// ---------- demo (human walkthrough) ----------

func newDemoCmd() *cobra.Command {
	var target string
	var skipLive bool
	c := &cobra.Command{
		Use:   "demo",
		Short: "Guided walkthrough: scan → doctor → fix preview → live experiment → webhook drill",
		RunE: func(cmd *cobra.Command, args []string) error {
			runDemo(target, skipLive)
			return nil
		},
	}
	c.Flags().StringVar(&target, "dir", "testdata", "directory to scan")
	c.Flags().BoolVar(&skipLive, "skip-live", false, "skip steps that call the Stripe API")
	return c
}

func runDemo(dir string, skipLive bool) {
	fmt.Println(banner("Dynamic Payment Methods — migration walkthrough",
		"docs.stripe.com/payments/dashboard-payment-methods, executed and verified locally"))

	total := 5
	// 1. scan
	fmt.Print(stepHeader(1, total, "Scan for hardcoded payment_method_types"))
	findings, scanned, parsed := scan(dir, dpmRule)
	sortFindings(findings)
	renderScan(ScanReport{Findings: findings, Stats: ScanStats{FilesScanned: scanned, FilesParsed: parsed, Skipped: scanned - parsed}})
	if len(findings) == 0 {
		fmt.Println("\n" + okLine("nothing to migrate — you're done"))
		return
	}

	// 2. doctor
	fmt.Print(stepHeader(2, total, "Judge findings against live account facts"))
	if skipLive {
		fmt.Println(infoLine("skipped (--skip-live)"))
	} else {
		var rep *DoctorReport
		err := withSpinner("Fetching account facts (read-only)", func() error {
			var derr error
			rep, derr = buildDoctorReport(findings, flagProfile)
			return derr
		})
		if err != nil {
			fmt.Println(warnLine("account facts unavailable: " + err.Error()))
		} else {
			renderDoctor(rep)
		}
	}

	// 3. fix preview
	fmt.Print(stepHeader(3, total, "Preview span-verified removals (dry-run)"))
	if rep, err := fixRun(dir, dpmRule, false); err == nil {
		renderFix(rep)
		fmt.Println(infoLine("apply for real with " + accentStyle.Render("dpm fix --apply "+dir)))
	}

	// 4. experiment
	fmt.Print(stepHeader(4, total, "Live experiment: Dashboard config drives the method list"))
	if skipLive {
		fmt.Println(infoLine("skipped (--skip-live)"))
	} else if confirm("Create an ephemeral TEST-mode config and diff two sessions?", flagYes) {
		var rep *ExperimentReport
		err := withSpinner("Creating config, sessions, and diffing", func() error {
			var derr error
			rep, derr = experimentRun(flagProfile, false)
			return derr
		})
		if err != nil {
			fmt.Println(warnLine(err.Error()))
		} else {
			renderExperiment(rep)
		}
	}

	// 5. drill
	fmt.Print(stepHeader(5, total, "Webhook drill: delayed-notification handling"))
	if skipLive {
		fmt.Println(infoLine("skipped (--skip-live)"))
	} else if confirm("Run stripe listen + trigger to verify the handler end to end?", flagYes) {
		var rep *DrillReport
		err := withSpinner("Listening, triggering, verifying signatures", func() error {
			var derr error
			rep, derr = drillRun()
			return derr
		})
		if err != nil {
			fmt.Println(warnLine(err.Error()))
		} else {
			renderDrill(rep)
		}
	}

	fmt.Println("\n" + banner("Walkthrough complete",
		"next: dpm fix --apply, then rerun dpm scan (exit 0 = migrated) — agents: dpm guide"))
}

// ---------- guide (agent playbook) ----------

func newGuideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guide",
		Short: "The agent playbook: step-by-step commands, JSON schemas, exit codes",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Print(agentGuide)
		},
	}
}

const agentGuide = `# DPM migration — agent playbook

Every command supports --json (single JSON object on stdout; logs on stderr)
and --yes (auto-approve confirmations). Exit codes: 0 clean/verified,
1 findings-present/not-verified, 2 operational error.

Run the steps in order; each step's output is evidence for the next.

## 1. Inventory
    dpm scan <dir> --json
Exit 0 → nothing to migrate; stop. Exit 1 → .findings[] each with
file/line/col, via (resolution mechanism), value, param.

## 2. Judge
    dpm doctor <dir> --json
Adds .account (event_api_versions, versions_at_or_after_cutoff,
dashboard_configured, methods_on) and per-finding verdict_class:
  CANDIDATE  safe removal target, preconditions met
  SKIP       deliberate restriction — do not remove
  REVIEW     needs human judgment (dynamic value / static multi)
  CAUTION/BLOCKED  account API version predates 2023-08-16 — removal
             without automatic_payment_methods[enabled]=true drops methods.
             STOP and surface to the human.
Only proceed to step 3 for CANDIDATE findings.

## 3. Preview the edit
    dpm fix <dir> --json
Dry-run. Per file: edits[] (byte spans + label), reparse must be "clean".

## 4. Apply
    dpm fix <dir> --apply --yes --json
Writes only reparse-clean files (.files[].written=true). Nothing else is
touched.

## 5. Confirm the code change
    dpm scan <dir> --json
Exit 0 proves the parameter is gone.

## 6. Confirm runtime behavior (optional but recommended)
    dpm experiment --yes --json     # Dashboard config drives sessions
    dpm drill --json                # delayed-notification webhooks round-trip
Both must report "verified": true. experiment creates an ephemeral TEST-mode
payment-method configuration and deactivates it; it never touches the
account's Default configuration.

## Cleanup safety net
    dpm cleanup <pmc_id>            # deactivate an orphaned ephemeral config
`

// ---------- cleanup / dump ----------

func newCleanupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cleanup <payment-method-configuration-id>",
		Short: "Deactivate an ephemeral demo configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := loadTestKey(flagProfile)
			if err != nil {
				fail(err)
			}
			if err := deactivate(key, args[0]); err != nil {
				fail(err)
			}
			fmt.Println(okLine("ephemeral config " + args[0] + " deactivated"))
			return nil
		},
	}
}

func newDumpCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "parse-dump [dir]",
		Short:  "Print S-expression trees (grammar debugging)",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			dumpTrees(argOr(args, "testdata"))
		},
	}
}

// ---------- helpers ----------

func argOr(args []string, def string) string {
	if len(args) > 0 {
		return args[0]
	}
	return def
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
}

func withSpinnerUnlessJSON(msg string, fn func() error) error {
	if flagJSON {
		fmt.Fprintf(os.Stderr, "%s...\n", msg)
		return fn()
	}
	return withSpinner(msg, fn)
}
