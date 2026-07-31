package main

// cmd.go — the command surface, shaped like the real product would be:
// generic verbs (doctor, fix, demo) with the migration TOPIC as an argument,
// mirroring the architecture (generic engine, rule packs as data).
//
//	stripe doctor [topic] [dir]   diagnose; degrades to scan-only without creds
//	stripe fix    [topic] [dir]   remediate; dry-run default, --apply writes
//	stripe demo   [topic]         guided walkthrough
//	stripe guide                  agent playbook
//
// scan/drill/experiment/cleanup are no longer commands: scan is doctor's
// credential-less degradation, the webhook drill is `doctor --live`, and the
// config experiment lives inside `demo` (cleanup stays as a hidden janitor).
//
// Every command supports --json (pure JSON on stdout, logs on stderr) and the
// exit-code contract: 0 clean/verified, 1 findings/not-verified, 2 error.

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

// packs is the registry of migration topics. Adding a migration means adding
// an entry here — the verbs never change.
var packs = map[string]Rule{
	"dpm": dpmRule,
}

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

// topicAndDir resolves the optional [topic] [dir] argument pair: an argument
// naming a known pack is the topic; anything else is the directory.
func topicAndDir(args []string) (string, string, error) {
	topic, dir := "dpm", "."
	var nonTopic []string
	for _, a := range args {
		if _, ok := packs[a]; ok {
			topic = a
		} else {
			nonTopic = append(nonTopic, a)
		}
	}
	// With two args, the first must be a topic; two non-topics means the
	// first was a typo'd topic, not a directory.
	if len(nonTopic) > 1 || (len(args) == 2 && len(nonTopic) == 2) {
		return "", "", fmt.Errorf("unknown topic %q (available: dpm)", nonTopic[0])
	}
	if len(nonTopic) == 1 {
		dir = nonTopic[0]
	}
	return topic, dir, nil
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "stripe",
		Short: "Migration doctor (demo) — diagnose, remediate, and verify API migrations",
		Long: `Diagnose and remediate Stripe API migrations. The engine is generic;
migrations are rule packs named by topic (currently: dpm — Dynamic Payment
Methods).

Humans: start with ` + "`stripe demo dpm`" + `. Agents: start with ` + "`stripe guide`" + `.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "machine-readable output on stdout, logs on stderr")
	root.PersistentFlags().StringVar(&flagProfile, "profile", "default", "Stripe CLI config profile for account access")
	root.PersistentFlags().BoolVarP(&flagYes, "yes", "y", false, "assume yes for confirmations (non-interactive)")

	root.AddCommand(newDoctorCmd(), newFixCmd(), newDemoCmd(), newGuideCmd(),
		newCleanupCmd(), newDumpCmd())
	return root
}

// ---------- doctor ----------

func newDoctorCmd() *cobra.Command {
	var live, offline bool
	c := &cobra.Command{
		Use:   "doctor [topic] [dir]",
		Short: "Diagnose a migration: code findings + account verdicts (read-only)",
		Long: `Scans the directory for the topic's findings and judges each against live
account facts (API versions in recent traffic, Dashboard configuration).
Without credentials it degrades to scan-only. --live additionally proves
runtime behavior (webhook round-trip via stripe listen/trigger).`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			topic, dir, err := topicAndDir(args)
			if err != nil {
				fail(err)
			}
			rule := packs[topic]

			findings, scanned, parsed, err := scan(dir, rule)
			if err != nil {
				fail(topicHint(err, dir))
			}
			sortFindings(findings)
			stats := ScanStats{FilesScanned: scanned, FilesParsed: parsed, Skipped: scanned - parsed}

			var rep *DoctorReport
			if offline {
				rep = scanOnlyReport(topic, findings, stats, "offline requested (--offline)")
			} else {
				err := withSpinnerUnlessJSON("Fetching account facts (read-only)", func() error {
					var derr error
					rep, derr = buildDoctorReport(findings, flagProfile)
					return derr
				})
				if err != nil {
					// Graceful degradation: no creds -> scan-only, clearly labeled.
					rep = scanOnlyReport(topic, findings, stats, err.Error())
				} else {
					rep.Topic = topic
					rep.Stats = stats
				}
			}

			// Code signals need no credentials — attach in every mode.
			rep.WebhookHandlers, rep.FrontendSignals = scanSignals(dir)

			failedLive := false
			if live {
				var drill *DrillReport
				err := withSpinnerUnlessJSON("Live check: webhook round-trip (listen + trigger)", func() error {
					var derr error
					drill, derr = drillRun()
					return derr
				})
				if err != nil {
					fail(err)
				}
				rep.LiveDrill = drill
				failedLive = !drill.Verified
			}

			if flagJSON {
				emitJSON(rep)
			} else {
				renderDoctor(rep)
			}
			if len(rep.Findings) > 0 || failedLive {
				exitWith(1)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&live, "live", false, "also run behavioral checks (spawns stripe listen/trigger)")
	c.Flags().BoolVar(&offline, "offline", false, "skip account facts; scan-only")
	return c
}

func scanOnlyReport(topic string, findings []Finding, stats ScanStats, why string) *DoctorReport {
	rep := &DoctorReport{Command: "doctor", Topic: topic, Degraded: why, Summary: map[string]int{}, Stats: stats}
	for _, f := range findings {
		intent := classifyIntent(f.Value)
		rep.Findings = append(rep.Findings, DoctorFinding{Finding: f, Intent: intent,
			Verdict: "UNKNOWN: account facts unavailable — " + why, Class: "UNKNOWN"})
		rep.Summary["UNKNOWN"]++
	}
	return rep
}

func renderDoctor(r *DoctorReport) {
	if r.Degraded != "" {
		fmt.Println(warnLine("scan-only: " + r.Degraded))
	} else {
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
	}

	if len(r.Findings) == 0 {
		fmt.Println("\n" + okLine(fmt.Sprintf("no findings — %d files scanned, %d parsed", r.Stats.FilesScanned, r.Stats.FilesParsed)))
	} else {
		// An account-level verdict (BLOCKED/CAUTION/UNKNOWN) is one fact, not
		// N facts: state it once as a headline and list findings compactly.
		shared := r.Findings[0].Verdict
		sharedClass := r.Findings[0].Class
		for _, f := range r.Findings {
			if f.Verdict != shared {
				shared = ""
				break
			}
		}
		accountLevel := shared != "" && (sharedClass == "BLOCKED" || sharedClass == "CAUTION" || sharedClass == "UNKNOWN")

		fmt.Printf("\n%s\n", titleStyle.Render(fmt.Sprintf("Findings (%d)", len(r.Findings))))
		if accountLevel {
			glyph := failLine
			if sharedClass != "BLOCKED" {
				glyph = warnLine
			}
			fmt.Println(glyph(titleStyle.Render("all findings: ") + shared))
			fmt.Println()
		}
		for _, f := range r.Findings {
			glyph := warnLine
			switch f.Class {
			case "CANDIDATE":
				glyph = okLine
			case "BLOCKED":
				glyph = failLine
			}
			fmt.Println(glyph(fmt.Sprintf("%s:%d  %s %s", f.File, f.Line, mutedStyle.Render("["+f.Intent+"]"), f.Value)))
			if !accountLevel {
				fmt.Println(kv("", f.Verdict))
			}
		}
		var sum []string
		for class, n := range r.Summary {
			sum = append(sum, fmt.Sprintf("%s ×%d", class, n))
		}
		sort.Strings(sum)
		fmt.Println("\n" + mutedStyle.Render("  "+strings.Join(sum, "  ")))
	}

	if r.WebhookHandlers != nil {
		h := r.WebhookHandlers
		fmt.Println("\n" + titleStyle.Render("Delayed-notification handlers in YOUR code"))
		line := func(name string, files []string) {
			if len(files) > 0 {
				fmt.Println(okLine(fmt.Sprintf("%-42s %s", name, mutedStyle.Render(files[0]))))
			} else {
				fmt.Println(warnLine(fmt.Sprintf("%-42s not found — required if you enable delayed methods (SEPA, ACH, ...)", name)))
			}
		}
		line("checkout.session.completed", h.Completed)
		line("checkout.session.async_payment_succeeded", h.AsyncSucceeded)
		line("checkout.session.async_payment_failed", h.AsyncFailed)
	}
	if len(r.FrontendSignals) > 0 {
		fmt.Println("\n" + titleStyle.Render("Frontend warnings"))
		for _, w := range r.FrontendSignals {
			fmt.Println(failLine(fmt.Sprintf("legacy Card Element signal %q in %s — dashboard-managed methods cannot render there", w.Signal, w.File)))
		}
	}

	if r.LiveDrill != nil {
		fmt.Println()
		renderDrill(r.LiveDrill)
	}
}

// ---------- fix ----------

func newFixCmd() *cobra.Command {
	var apply, all bool
	c := &cobra.Command{
		Use:   "fix [topic] [dir]",
		Short: "Remediate findings: span-verified removals (dry-run; --apply writes)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			topic, dir, err := topicAndDir(args)
			if err != nil {
				fail(err)
			}
			if apply && !confirm("Apply removals in place? (only reparse-clean files are written)", flagYes) {
				fmt.Println(infoLine("aborted; nothing written"))
				exitWith(1)
			}
			rep, err := fixRun(dir, packs[topic], apply, all)
			if err != nil {
				fail(topicHint(err, dir))
			}
			rep.Topic = topic
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
	c.Flags().BoolVar(&all, "all", false, "include dynamic/deliberate findings the gate would skip")
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

func renderDrill(r *DrillReport) {
	fmt.Println(titleStyle.Render("Live check: delayed-notification webhooks"))
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
		fmt.Println(okLine(titleStyle.Render("verified: the handler round-trips end to end")))
	} else {
		fmt.Println(warnLine("not fully verified" + mutedStyle.Render(" — "+r.Note)))
	}
}

// ---------- demo ----------

func newDemoCmd() *cobra.Command {
	var dir string
	var skipLive bool
	c := &cobra.Command{
		Use:   "demo [topic]",
		Short: "Guided walkthrough: diagnose → remediation preview → live proof",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			topic := "dpm"
			if len(args) > 0 {
				topic = args[0]
			}
			if _, ok := packs[topic]; !ok {
				fail(fmt.Errorf("unknown topic %q", topic))
			}
			runDemo(topic, dir, skipLive)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", ".", "directory to scan")
	c.Flags().BoolVar(&skipLive, "skip-live", false, "skip steps that call the Stripe API")
	return c
}

func runDemo(topic, dir string, skipLive bool) {
	rule := packs[topic]
	fmt.Println(banner("Dynamic Payment Methods — migration walkthrough",
		"docs.stripe.com/payments/dashboard-payment-methods, executed and verified locally"))

	total := 4
	// 1. doctor (code + account together — the product's core loop)
	fmt.Print(stepHeader(1, total, "Diagnose: code findings judged against account facts"))
	findings, scanned, parsed, err := scan(dir, rule)
	if err != nil {
		fail(topicHint(err, dir))
	}
	sortFindings(findings)
	stats := ScanStats{FilesScanned: scanned, FilesParsed: parsed, Skipped: scanned - parsed}
	if len(findings) == 0 {
		fmt.Println(okLine("no findings — nothing to migrate"))
		return
	}
	var rep *DoctorReport
	if skipLive {
		rep = scanOnlyReport(topic, findings, stats, "--skip-live")
	} else {
		err := withSpinner("Fetching account facts (read-only)", func() error {
			var derr error
			rep, derr = buildDoctorReport(findings, flagProfile)
			return derr
		})
		if err != nil {
			rep = scanOnlyReport(topic, findings, stats, err.Error())
		} else {
			rep.Topic = topic
			rep.Stats = stats
		}
	}
	rep.WebhookHandlers, rep.FrontendSignals = scanSignals(dir)
	renderDoctor(rep)

	// 2. remediation preview
	fmt.Print(stepHeader(2, total, "Remediation preview (dry-run, span-verified)"))
	if fr, err := fixRun(dir, rule, false, false); err == nil {
		renderFix(fr)
		fmt.Println(infoLine("apply for real with " + accentStyle.Render(fmt.Sprintf("stripe fix %s %s --apply", topic, dir))))
	}

	// 3. live experiment
	fmt.Print(stepHeader(3, total, "Live proof: Dashboard config drives the method list"))
	if skipLive {
		fmt.Println(infoLine("skipped (--skip-live)"))
	} else if confirm("Create an ephemeral TEST-mode config and diff two sessions?", flagYes) {
		var er *ExperimentReport
		err := withSpinner("Creating config, sessions, and diffing", func() error {
			var derr error
			er, derr = experimentRun(flagProfile, false)
			return derr
		})
		if err != nil {
			fmt.Println(warnLine(err.Error()))
		} else {
			renderExperiment(er)
		}
	}

	// 4. live webhook check
	fmt.Print(stepHeader(4, total, "Live proof: delayed-notification webhooks round-trip"))
	if skipLive {
		fmt.Println(infoLine("skipped (--skip-live)"))
	} else if confirm("Run stripe listen + trigger to verify the handler end to end?", flagYes) {
		var dr *DrillReport
		err := withSpinner("Listening, triggering, verifying signatures", func() error {
			var derr error
			dr, derr = drillRun()
			return derr
		})
		if err != nil {
			fmt.Println(warnLine(err.Error()))
		} else {
			renderDrill(dr)
		}
	}

	fmt.Println("\n" + banner("Walkthrough complete",
		fmt.Sprintf("next: stripe fix %s --apply, then stripe doctor %s (exit 0 = migrated) — agents: stripe guide", topic, topic)))
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
}

// ---------- guide ----------

func newGuideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guide",
		Short: "The agent playbook: step-by-step commands, JSON schemas, exit codes",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Print(agentGuide)
		},
	}
}

const agentGuide = `# Migration doctor — agent playbook

Verbs are generic; the migration is a topic argument (currently: dpm).
Every command supports --json (single JSON object on stdout; logs on stderr)
and --yes (auto-approve confirmations). Exit codes: 0 clean/verified,
1 findings-present/not-verified, 2 operational error.

## 1. Diagnose
    stripe doctor dpm <dir> --json
Exit 0 → nothing to migrate; stop. Exit 1 → .findings[] each with
file/line/col, via (resolution mechanism), value, and verdict_class:
  CANDIDATE  safe removal target, account preconditions met
  SKIP       deliberate restriction — do not remove
  REVIEW     needs human judgment (dynamic value / static multi)
  CAUTION/BLOCKED  account API version predates 2023-08-16 — removal
             without automatic_payment_methods[enabled]=true drops
             methods. STOP and surface to the human.
  UNKNOWN    account facts unavailable (.degraded says why) — treat
             as REVIEW.
.account carries the evidence (event_api_versions, dashboard_configured).
Only proceed to step 2 for CANDIDATE findings.

Also in the doctor report, credentials or not:
  .webhook_handlers  does YOUR code mention the three delayed-notification
                     event types (all_present should be true before enabling
                     delayed methods)
  .frontend_warnings legacy Card Element signals — dashboard-managed methods
                     cannot render there; server-side removal alone strands
                     them. Surface these to the human.

## 2. Preview the remediation
    stripe fix dpm <dir> --json
Dry-run. Per file: edits[] (byte spans + label); reparse must be "clean".
The gate skips dynamic values and deliberate single-method restrictions
(.skipped, with reasons) — that is intentional; --all overrides, but only
after a human reviews each skipped finding.

## 3. Apply
    stripe fix dpm <dir> --apply --yes --json
Writes only reparse-clean files (.files[].written=true). A per-file write
failure is recorded in .files[].error and does not abort the rest.

## 4. Confirm the code change
    stripe doctor dpm <dir> --json
Exit 0 proves the parameter is gone.

## 5. Confirm runtime behavior (recommended)
    stripe doctor dpm --live --json
Adds .live_drill (webhook round-trip via stripe listen/trigger); require
.live_drill.verified == true.

## Notes
- The demo's config experiment (stripe demo dpm) creates an ephemeral
  TEST-mode payment-method configuration and deactivates it; it never
  touches the account's Default configuration.
- Orphaned ephemeral config? stripe cleanup <pmc_id>.
`

// ---------- hidden: cleanup / parse-dump ----------

func newCleanupCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "cleanup <payment-method-configuration-id>",
		Short:  "Deactivate an ephemeral demo configuration",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
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
			d := "testdata"
			if len(args) > 0 {
				d = args[0]
			}
			dumpTrees(d)
		},
	}
}

// topicHint decorates a scan error when the "directory" looks like a typo'd
// topic name (e.g. `doctor dmp`).
func topicHint(err error, dir string) error {
	for name := range packs {
		if dir != name && editDistanceAtMost2(dir, name) {
			return fmt.Errorf("%w (did you mean topic %q?)", err, name)
		}
	}
	return err
}

func editDistanceAtMost2(a, b string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > 2 || len(b) > 12 {
		return false
	}
	// tiny DP is overkill at these sizes; do full Levenshtein
	prev := make([]int, len(a)+1)
	cur := make([]int, len(a)+1)
	for i := range prev {
		prev[i] = i
	}
	for j := 1; j <= len(b); j++ {
		cur[0] = j
		for i := 1; i <= len(a); i++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[i] = min(min(cur[i-1]+1, prev[i]+1), prev[i-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(a)] <= 2
}

// ---------- helpers ----------

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
