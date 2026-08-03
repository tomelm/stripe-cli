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
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var (
	flagJSON          bool
	flagProfile       string
	flagStripeAccount string
	flagYes           bool
)

// Pack pairs a rule with its declared signals (expected webhook events,
// legacy frontend tokens, package version floors). Adding a migration means
// adding an entry here — the verbs never change.
type Pack struct {
	Rule    Rule
	Signals *PackSignals
	Triage  []TriageBranch
}

var packs = map[string]Pack{
	"dpm":               {Rule: dpmRule, Signals: &dpmSignals},
	"tax-percent":       {Rule: taxPercentRule},
	"collection-method": {Rule: collectionMethodRule},
	"prorate":           {Rule: prorateRule},
	"source-types":      {Rule: sourceTypesRule},
	"ewcs":              {Rule: ewcsRule, Signals: &ewcsSignals},
	"flex":              {Rule: flexRule},
	"pe":                {Rule: peRule, Signals: &peSignals},
	"ct":                {Rule: ctRule, Signals: &ctSignals},
	"elements":          {Rule: elementsRule, Triage: elementsTriage},
}

// topicList renders the registry for help and error text.
func topicList() string {
	var names []string
	for n := range packs {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
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
		return "", "", fmt.Errorf("unknown topic %q (available: %s)", nonTopic[0], topicList())
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
migrations are rule packs named by topic.

Topics: dpm (fixable) · elements (WHICH Payment Element migration applies —
start here if unsure) · pe · ewcs · ct (the three Payment Element guides) ·
flex · tax-percent · collection-method · prorate · source-types (advise-only).

Humans: start with ` + "`stripe demo dpm`" + `. Agents: start with ` + "`stripe guide`" + `.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "machine-readable output on stdout, logs on stderr")
	root.PersistentFlags().StringVar(&flagProfile, "profile", "default", "Stripe CLI config profile for account access")
	root.PersistentFlags().StringVar(&flagStripeAccount, "stripe-account", "", "Connect: connected account (acct_...) whose configuration governs direct charges")
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
			rule := packs[topic].Rule

			findings, scanned, parsed, err := scan(dir, rule)
			if err != nil {
				fail(topicHint(err, dir))
			}
			sortFindings(findings)
			stats := ScanStats{FilesScanned: scanned, FilesParsed: parsed, Skipped: scanned - parsed}

			var rep *DoctorReport
			if offline {
				rep = scanOnlyReport(topic, rule, findings, stats, "offline requested (--offline)")
			} else {
				err := withSpinnerUnlessJSON("Fetching account facts (read-only)", func() error {
					var derr error
					rep, derr = buildDoctorReport(findings, flagProfile, flagStripeAccount, rule)
					return derr
				})
				if err != nil {
					// Graceful degradation: no creds -> scan-only, clearly labeled.
					rep = scanOnlyReport(topic, rule, findings, stats, err.Error())
				} else {
					rep.Topic = topic
					rep.Stats = stats
				}
			}

			// Code signals need no credentials — attach in every mode.
			rep.WebhookHandlers, rep.FrontendSignals, rep.ManifestChecks = scanSignals(dir, packs[topic].Signals)
			rep.Triage = scanTriage(dir, packs[topic].Triage)

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

func scanOnlyReport(topic string, rule Rule, findings []Finding, stats ScanStats, why string) *DoctorReport {
	rep := &DoctorReport{Command: "doctor", Topic: topic, Degraded: why, Summary: map[string]int{}, Stats: stats}
	for _, f := range findings {
		intent := classifyIntent(f.Value)
		v, class := "UNKNOWN: account facts unavailable — "+why, "UNKNOWN"
		if rule.Action == "advise" {
			// An advise verdict is a version fact, not an account fact — it
			// needs no credentials.
			v, class = "ADVISE: "+rule.Message, "ADVISE"
			if rule.IntroducedIn != "" {
				v += " (API " + rule.IntroducedIn + ")"
			}
		}
		rep.Findings = append(rep.Findings, DoctorFinding{Finding: f, Intent: intent, Verdict: v, Class: class})
		rep.Summary[class]++
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
		if len(a.Unavailable) > 0 {
			fmt.Println(warnLine("toggled ON but NOT available (capability inactive — will not render): " + strings.Join(a.Unavailable, ", ")))
		}
		if a.Configs > 1 {
			fmt.Println(infoLine(fmt.Sprintf("%d active configurations — verdicts use the default; call sites passing payment_method_configuration explicitly are governed by that config instead", a.Configs)))
		}
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

	if r.WebhookHandlers != nil && len(r.WebhookHandlers.Events) > 0 {
		fmt.Println("\n" + titleStyle.Render("Expected webhook handlers in YOUR code"))
		for _, e := range r.WebhookHandlers.Events {
			if e.Present {
				fmt.Println(okLine(fmt.Sprintf("%-46s %s", e.Event, mutedStyle.Render(e.Files[0]))))
			} else {
				fmt.Println(warnLine(fmt.Sprintf("%-46s not found in scanned code", e.Event)))
			}
		}
	}
	if len(r.FrontendSignals) > 0 {
		fmt.Println("\n" + titleStyle.Render("Frontend warnings"))
		for _, w := range r.FrontendSignals {
			note := w.Note
			if note == "" {
				note = "legacy client-side token"
			}
			fmt.Println(failLine(fmt.Sprintf("%q in %s — %s", w.Signal, w.File, note)))
		}
	}
	if len(r.Triage) > 0 {
		fmt.Println("\n" + titleStyle.Render("Which migration applies"))
		for _, t := range r.Triage {
			fmt.Println(okLine(titleStyle.Render("detected: ") + t.Detected))
			fmt.Println(kv("", accentStyle.Render(t.Recommend)))
			max := len(t.Evidence)
			if max > 3 {
				max = 3
			}
			for _, ev := range t.Evidence[:max] {
				fmt.Println(kv("", mutedStyle.Render(fmt.Sprintf("%s  (%s)", ev.File, ev.Token))))
			}
		}
	}
	if len(r.ManifestChecks) > 0 {
		fmt.Println("\n" + titleStyle.Render("Package version floors"))
		for _, m := range r.ManifestChecks {
			switch {
			case m.Found == "":
				fmt.Println(infoLine(fmt.Sprintf("%-28s not in package.json (floor %s applies only if used)", m.Package, m.Floor)))
			case m.OK:
				fmt.Println(okLine(fmt.Sprintf("%-28s %s (floor %s)", m.Package, m.Found, m.Floor)))
			default:
				fmt.Println(failLine(fmt.Sprintf("%-28s %s is below required %s", m.Package, m.Found, m.Floor)))
			}
		}
	}

	if r.LiveDrill != nil {
		fmt.Println()
		renderDrill(r.LiveDrill)
	}
}

// ---------- fix ----------

func newFixCmd() *cobra.Command {
	var apply, all, offline bool
	var returnURL string
	c := &cobra.Command{
		Use:   "fix [topic] [dir]",
		Short: "Remediate findings: span-verified removals (dry-run; --apply writes)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			topic, dir, err := topicAndDir(args)
			if err != nil {
				fail(err)
			}
			rule := packs[topic].Rule
			if returnURL != "" {
				if err := validateReturnURL(returnURL); err != nil {
					fail(err)
				}
			}
			// Version fork: rules with a companion consult account facts to
			// choose remove vs replace before any span is computed.
			var dec *companionDecision
			if rule.Companion != nil {
				_ = withSpinnerUnlessJSON("Checking account API versions (read-only)", func() error {
					dec = resolveCompanion(rule, flagProfile, flagStripeAccount, dir, offline)
					return nil
				})
				dec.returnURL = returnURL
			}
			// Disclosure BEFORE consent: --apply first computes and renders
			// the dry-run (which sites get which companion variant, what the
			// gate skips), offers a return_url when pinned/gated confirm
			// sites exist, and only then asks to write.
			if apply && !flagJSON && !flagYes {
				preview, err := fixRun(dir, rule, false, all, dec)
				if err != nil {
					fail(topicHint(err, dir))
				}
				preview.Topic = topic
				renderFix(preview)
				if dec != nil && dec.returnURL == "" && wantsReturnURL(preview) {
					if url := promptLine("return_url for the server-side-confirmation site(s) above (blank = keep the allow_redirects:\"never\" pin / leave gated sites gated):"); url != "" {
						if err := validateReturnURL(url); err != nil {
							fail(err)
						}
						dec.returnURL = url
						fmt.Println(infoLine("recomputing with return_url " + url))
					}
				}
				if !confirm("Apply the changes above? (only reparse-clean files are written)", flagYes) {
					fmt.Println(infoLine("aborted; nothing written"))
					exitWith(1)
				}
			} else if apply && !confirm("Apply removals in place? (only reparse-clean files are written)", flagYes) {
				fmt.Println(infoLine("aborted; nothing written"))
				exitWith(1)
			}
			rep, err := fixRun(dir, rule, apply, all, dec)
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
	c.Flags().BoolVar(&offline, "offline", false, "skip account lookups (companion rules then insert, the safe-at-any-version choice)")
	c.Flags().StringVar(&returnURL, "return-url", "", "return_url to add at server-side-confirmation sites (enables redirect-based payment methods; without it those sites get allow_redirects:\"never\")")
	return c
}

// validateReturnURL rejects values that would break the generated code or
// smuggle extra API parameters: the URL is spliced into source verbatim, so
// quotes, backslashes, whitespace, and non-http schemes are refused outright.
func validateReturnURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("--return-url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("--return-url must be http(s), got %q", s)
	}
	if u.Host == "" {
		return fmt.Errorf("--return-url has no host: %q", s)
	}
	if strings.ContainsAny(s, "'\"\\` \t\n\r") {
		return fmt.Errorf("--return-url contains characters that cannot be spliced into source code: %q", s)
	}
	return nil
}

// wantsReturnURL reports whether the preview found server-side-confirmation
// sites that a return_url would improve: pinned sites, or confirm-redirect
// gated skips.
func wantsReturnURL(r *FixReport) bool {
	if r.Companion != nil && len(r.Companion.PinnedSites) > 0 {
		return true
	}
	for _, sk := range r.Skipped {
		if sk.Intent == "confirm-redirect" {
			return true
		}
	}
	return false
}

func renderFix(r *FixReport) {
	mode := "dry-run (nothing written)"
	if r.Applied {
		mode = "APPLIED"
	}
	fmt.Println(titleStyle.Render("Removals — " + mode))
	if c := r.Companion; c != nil {
		who := ""
		if c.Account != "" {
			who = " [" + c.Account + "]"
		}
		if c.Mode == "insert" {
			fmt.Println(infoLine("companion: inserting " + c.Param + who + " — " + c.Reason))
		} else {
			fmt.Println(infoLine("companion: not needed" + who + " — " + c.Reason))
		}
		for _, p := range c.PinnedSites {
			fmt.Println(warnLine(fmt.Sprintf("pinned allow_redirects:\"never\" at %s:%d — redirect-based methods stay off at this site without a return_url", p.File, p.Line)))
		}
		for _, n := range c.Notes {
			fmt.Println(warnLine(n))
		}
	}
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
		delta := fmt.Sprintf("−%d bytes", f.BytesRemoved)
		if f.BytesAdded > 0 {
			delta = fmt.Sprintf("−%d/+%d bytes", f.BytesRemoved, f.BytesAdded)
		}
		line := fmt.Sprintf("%s  %s  %s", f.Path, delta, mutedStyle.Render(strings.Join(ls, ", ")))
		switch {
		case f.Reparse == "error":
			fmt.Println(failLine(line + "  reparse ERROR — not written"))
		case f.Written:
			fmt.Println(okLine(line + "  written"))
		default:
			fmt.Println(okLine(line + "  reparse clean"))
		}
	}
	for _, sk := range r.Skipped {
		fmt.Println(warnLine(fmt.Sprintf("skipped %s:%d %s — %s", sk.File, sk.Line, mutedStyle.Render("["+sk.Intent+"]"), sk.Reason)))
	}
	if len(r.Files) == 0 && len(r.Skipped) == 0 {
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
	rule := packs[topic].Rule
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
		rep = scanOnlyReport(topic, rule, findings, stats, "--skip-live")
	} else {
		err := withSpinner("Fetching account facts (read-only)", func() error {
			var derr error
			rep, derr = buildDoctorReport(findings, flagProfile, flagStripeAccount, rule)
			return derr
		})
		if err != nil {
			rep = scanOnlyReport(topic, rule, findings, stats, err.Error())
		} else {
			rep.Topic = topic
			rep.Stats = stats
		}
	}
	rep.WebhookHandlers, rep.FrontendSignals, rep.ManifestChecks = scanSignals(dir, packs[topic].Signals)
	rep.Triage = scanTriage(dir, packs[topic].Triage)
	renderDoctor(rep)

	// 2. remediation preview
	fmt.Print(stepHeader(2, total, "Remediation preview (dry-run, span-verified)"))
	var dec *companionDecision
	if rule.Companion != nil {
		dec = resolveCompanion(rule, flagProfile, flagStripeAccount, dir, skipLive)
	}
	if fr, err := fixRun(dir, rule, false, false, dec); err == nil {
		renderFix(fr)
		// The recommended command must reproduce THIS preview: when the
		// preview's fork was decided offline, say so, or a live re-resolve
		// could choose the other branch and apply a different edit.
		applyCmd := fmt.Sprintf("stripe fix %s %s --apply", topic, dir)
		if skipLive {
			applyCmd += " --offline"
		}
		fmt.Println(infoLine("apply for real with " + accentStyle.Render(applyCmd)))
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

Verbs are generic; the migration is a topic argument. Topics:
  dpm                remove payment_method_types (fixable: doctor -> fix -> doctor)
  elements           WHICH Payment Element migration applies? Run this first
                     when unsure: .triage detects each from-state with file
                     evidence — Charges-era (prerequisite migration), legacy
                     Card Element (-> pe or ewcs), PaymentMethod server
                     handoff (-> ct, mandatory: from-state is unsupported),
                     and already-migrated markers
  pe                 legacy per-PM Elements -> Payment Element (advise;
                     signals: confirm<PM>Payment/Setup family, card Element;
                     expected events payment_intent.succeeded/processing/
                     payment_failed; note: Stripe prefers ewcs for most)
  ct                 PaymentMethod handoff -> Confirmation Tokens (advise;
                     anchor mandate_data@PI create/confirm; signals:
                     createPaymentMethod, paymentMethodId handoff)
  ewcs               Elements with Checkout Sessions readiness report (advise;
                     recommendation-class — no version gate; the doctor report
                     carries the machine-checkable prerequisites: .findings are
                     the create calls to migrate, .webhook_handlers diffs the
                     four expected checkout.session.* events, .frontend_warnings
                     lists from-state client tokens with their replacements,
                     .manifest_checks enforces @stripe/stripe-js>=8 and
                     @stripe/react-stripe-js>=5; the actual rewrite is agent/
                     human work per the docs link)
  flex               flexible payment features beta->GA (advise; account-gated,
                     not version-gated: anchors the incremental-auth rename and
                     final_capture semantics; the other features are param
                     ADDITIONS a presence scanner cannot see — follow the doc)
  tax-percent        tax_percent removed 2020-08-27 (advise: needs TaxRate objects)
  collection-method  billing -> collection_method rename, 2019-10-17 (advise)
  prorate            prorate -> proration_behavior, 2020-08-27 (advise)
  source-types       allowed_source_types -> payment_method_types, 2019-02-11 (advise)
Advise topics: doctor detects (verdict_class ADVISE, works offline) and the
message names the replacement; fix exits 2 by design — apply the rename per
the docs, then re-run doctor for exit 0. For source-types, run topic dpm
afterwards: the renamed param is dpm's target.
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
             methods. fix handles this: it forks by version and
             REPLACES the parameter with the companion there (see
             step 2's .companion). Surface the verdict to the human
             but the code change remains safe to preview.
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
.companion reports the version fork: mode "insert" means removals of
payment_method_types on PaymentIntents/SetupIntents become REPLACEMENTS
that splice in automatic_payment_methods[enabled]=true (required below
the 2023-08-16 cutoff, harmless above it); mode "omit" means account
traffic is all at/after the cutoff and plain removal preserves behavior.
.companion.reason carries the account evidence; --offline forces the
insert branch (the only choice correct at any version). Checkout
Sessions/Payment Links never get the insert (no such parameter there).
Server-side confirmation: a create with confirm:true and no return_url
would 400 at runtime once automatic_payment_methods is in effect —
which is BOTH branches of the fork (inserted below the cutoff,
default-on above it), so confirm sites always get explicit handling:
  --return-url <url> given → companion + return_url (keeps redirect
    methods; the URL is validated — http(s), no quotes/spaces)
  old hardcoded list named redirect-based methods (ideal, bancontact,
    giropay, sofort, p24, ...) and no return_url → the site is GATED
    (.skipped intent "confirm-redirect"): pinning would silently drop
    a method the merchant used; removal alone would 400. ASK THE HUMAN
    for a return_url and rerun — that is the intended resolution.
  otherwise → companion + allow_redirects:"never";
    .companion.pinned_sites lists exactly which file:line got the pin.
.companion.account names the account whose facts decided the fork. A
Stripe-Version pinned in CODE below the cutoff overrides an omit
verdict (the events census only reflects the account default).
Also in doctor's .account: enabled_but_unavailable lists methods
toggled ON whose capability is inactive — they will NOT render; treat
them as missing when judging method coverage.
Connect: for direct charges (Stripe-Account header in the user's code),
pass --stripe-account acct_... so the CONNECTED account's configuration
and traffic govern the fork and the doctor's verdicts.
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
