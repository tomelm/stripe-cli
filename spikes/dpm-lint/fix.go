package main

// fix.go — span-verified remediation. For each finding, compute the span that
// removes the whole parameter entry (pair + separator, or builder link, or
// whole statement), apply the edits in memory, REPARSE the result, and only
// write files whose reparse is clean.
//
// Rules with a Companion fork by account API version: at/after the rule's
// cutoff the parameter is simply removed; below it (or when the version is
// unknowable) the removal becomes a REPLACEMENT that inserts the companion
// parameter in the same spot, because bare removal would silently change
// behavior there (verified live: a bare pre-cutoff PaymentIntent resolves to
// card-only).

import (
	"fmt"
	"os"
	"sort"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

type span struct {
	start, end uint32
	label      string
	// site describes the removal-site shape for companion insertion: the pair
	// node kind for pair languages, "statement"/"chain-link" for Java.
	site string
	// receiver is the builder variable text for Java statement sites.
	receiver string
	// replace, when non-empty, turns the deletion into a splice.
	replace string
}

// removalSpan computes the byte range that deletes a matched parameter
// entirely, per language shape.
func removalSpan(key *ts.Node, spec langSpec, lang *ts.Language, src []byte) (span, bool) {
	if spec.pairKinds == nil {
		// Java builder: the key is the method name of an invocation link.
		var inv *ts.Node
		for cur := key.Parent(); cur != nil; cur = cur.Parent() {
			if containsStr(spec.anchorKinds, cur.Type(lang)) {
				inv = cur
				break
			}
		}
		if inv == nil {
			return span{}, false
		}
		// Standalone statement (paramsBuilder.addX("card");) → remove the
		// whole statement; chain link (.addX("card")) → remove from the end
		// of the receiver through the end of this link.
		recv := inv.NamedChild(0)
		if p := inv.Parent(); p != nil && p.Type(lang) == "expression_statement" {
			receiver := ""
			if recv != nil {
				receiver = string(src[recv.StartByte():recv.EndByte()])
			}
			return span{start: p.StartByte(), end: p.EndByte(), label: "statement", site: "statement", receiver: receiver}, true
		}
		if recv == nil {
			return span{}, false
		}
		return span{start: recv.EndByte(), end: inv.EndByte(), label: "chain-link", site: "chain-link"}, true
	}

	// Pair-shaped languages: the enclosing pair node plus one separator.
	var pair *ts.Node
	for cur := key.Parent(); cur != nil; cur = cur.Parent() {
		if containsStr(spec.pairKinds, cur.Type(lang)) {
			pair = cur
			break
		}
	}
	if pair == nil {
		return span{}, false
	}
	site := pair.Type(lang)
	s, e := pair.StartByte(), pair.EndByte()
	// Prefer swallowing the trailing comma; else the leading one.
	if i := skipWS(src, int(e), +1); i < len(src) && src[i] == ',' {
		return span{start: s, end: uint32(i + 1), label: "pair+trailing-comma", site: site}, true
	}
	if i := skipWS(src, int(s)-1, -1); i >= 0 && src[i] == ',' {
		return span{start: uint32(i), end: e, label: "pair+leading-comma", site: site}, true
	}
	return span{start: s, end: e, label: "pair", site: site}, true
}

// expandToLine widens a pure removal to whole lines when only whitespace
// would remain around it, so deletions never leave blank or whitespace-only
// lines behind (multi-line spans expand to their outer line boundaries).
func expandToLine(sp span, src []byte) span {
	ls := int(sp.start)
	for ls > 0 && src[ls-1] != '\n' {
		ls--
	}
	le := int(sp.end)
	for le < len(src) && src[le] != '\n' {
		le++
	}
	if !allWS(src[ls:sp.start]) || !allWS(src[sp.end:le]) {
		return sp
	}
	if le < len(src) {
		le++ // swallow the newline
	}
	out := sp
	out.start, out.end, out.label = uint32(ls), uint32(le), sp.label+"+line"
	return out
}

func allWS(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' {
			return false
		}
	}
	return true
}

// ---------- companion (version-forked replace) ----------

// companionDecision is the resolved version fork: insert the companion, or
// plain-remove because the account's traffic is entirely at/after the cutoff.
type companionDecision struct {
	insert bool
	reason string
	oldest string
}

// resolveCompanion decides the fork from account facts. Every unknowable
// branch (offline, no credentials, no recent events) resolves to INSERT: the
// companion is semantically a no-op at/after the cutoff and load-bearing
// below it, so inserting is the only choice that is correct at any version.
func resolveCompanion(rule Rule, profile string, offline bool) *companionDecision {
	cutoff := rule.IntroducedIn
	unknown := func(why string) *companionDecision {
		return &companionDecision{insert: true,
			reason: why + " — traffic API versions unknown; inserting " + rule.Companion.Param + " (a no-op at/after " + cutoff + ", required below it)"}
	}
	if offline {
		return unknown("--offline")
	}
	key, err := loadTestKey(profile)
	if err != nil {
		return unknown("no credentials")
	}
	var facts *accountFacts
	if facts, err = fetchAccountFacts(key); err != nil {
		return unknown("account lookup failed")
	}
	switch {
	case facts.NoEvents:
		return unknown("no recent events")
	case facts.VersionsOK:
		return &companionDecision{insert: false, oldest: facts.OldestVersion,
			reason: "all recent traffic runs at/after " + cutoff + " — " + rule.Companion.Param + " is default-enabled there; plain removal is behavior-preserving"}
	default:
		return &companionDecision{insert: true, oldest: facts.OldestVersion,
			reason: "recent traffic runs below " + cutoff + " (oldest " + facts.OldestVersion + ") — bare removal would silently drop methods; inserting " + rule.Companion.Param}
	}
}

// companionResourceFor maps a finding's resolved anchor text onto one of the
// companion's eligible API resources ("" = not eligible, e.g. a Checkout
// Session, which has no automatic_payment_methods parameter).
func companionResourceFor(anchor string, resources []string) string {
	for _, r := range resources {
		for _, tok := range []string{pascalSingular(r), lowerCamelPlural(r), r} {
			if strings.Contains(anchor, tok) {
				return r
			}
		}
	}
	return ""
}

// companionText renders the language-correct insertion of
// automatic_payment_methods[enabled]=true for a removal site. site is the
// pair node kind (or statement/chain-link for Java builders); resource picks
// the typed SDKs' params-class prefix.
func companionText(spec langSpec, site, resource, receiver string) (string, bool) {
	prefix := pascalSingular(resource) // PaymentIntent | SetupIntent
	switch spec.name {
	case "ruby":
		return "automatic_payment_methods: {enabled: true}", true
	case "python":
		if site == "keyword_argument" {
			return `automatic_payment_methods={"enabled": True}`, true
		}
		return `"automatic_payment_methods": {"enabled": True}`, true
	case "php":
		return "'automatic_payment_methods' => ['enabled' => true]", true
	case "javascript", "typescript", "tsx":
		return "automatic_payment_methods: {enabled: true}", true
	case "go":
		return "AutomaticPaymentMethods: &stripe." + prefix + "AutomaticPaymentMethodsParams{Enabled: stripe.Bool(true)}", true
	case "csharp":
		return "AutomaticPaymentMethods = new " + prefix + "AutomaticPaymentMethodsOptions { Enabled = true }", true
	case "java":
		call := ".setAutomaticPaymentMethods(" + prefix + "CreateParams.AutomaticPaymentMethods.builder().setEnabled(true).build())"
		switch site {
		case "statement":
			if receiver == "" {
				return "", false
			}
			return receiver + call + ";", true
		case "chain-link":
			return call, true
		}
	}
	return "", false
}

// spliceFor wraps the companion text with the separator the span swallowed,
// so the replacement drops into the exact bytes the removal vacated.
func spliceFor(label, text string) string {
	switch label {
	case "pair+trailing-comma":
		return text + ","
	case "pair+leading-comma":
		return "," + text
	}
	return text
}

func skipWS(src []byte, i, dir int) int {
	for i >= 0 && i < len(src) && (src[i] == ' ' || src[i] == '\t' || (dir > 0 && src[i] == '\n')) {
		i += dir
	}
	return i
}

// fixRun computes removal spans per file, applies them in memory, reparses,
// and — only when apply is true AND the reparse is clean — writes the file.
// A file that fails reparse is never written. dec carries the resolved
// companion fork (nil when the rule has no companion).
func fixRun(root string, rule Rule, apply, includeAll bool, dec *companionDecision) (*FixReport, error) {
	if rule.Action != "remove" {
		return nil, fmt.Errorf("rule %s is action=%q: it detects and advises but has no automatic fix — run `doctor` and follow %s", rule.ID, rule.Action, rule.Docs)
	}
	type fileEdit struct {
		spec  langSpec
		spans []span
	}
	edits := map[string]*fileEdit{}
	// Java repeats the same param as N builder statements/links on ONE
	// builder; only the first replacement per builder may insert the
	// companion or the call would set it twice.
	javaSeen := map[string]bool{}

	findings, _, _, err := scan(root, rule)
	if err != nil {
		return nil, err
	}
	report := &FixReport{Command: "fix", Applied: apply, AllClean: true}
	if rule.Companion != nil && dec != nil {
		mode := "omit"
		if dec.insert {
			mode = "insert"
		}
		report.Companion = &CompanionReport{Param: rule.Companion.Param, Mode: mode, Reason: dec.reason, OldestVersion: dec.oldest}
	}
	for _, f := range findings {
		// Gate: dynamic values and deliberate single-method restrictions are
		// never auto-removed unless --all — the doctor's own taxonomy says
		// they need human judgment.
		if !includeAll {
			switch intent := classifyIntent(f.Value); intent {
			case "dynamic":
				report.Skipped = append(report.Skipped, SkippedFinding{File: f.File, Line: f.Line, Intent: intent,
					Reason: "value is computed at runtime — review the routing logic (use --all to override)"})
				continue
			case "deliberate":
				report.Skipped = append(report.Skipped, SkippedFinding{File: f.File, Line: f.Line, Intent: intent,
					Reason: "single-method restriction looks intentional — consider excluded_payment_method_types (use --all to override)"})
				continue
			}
		}
		spec := specs[strings.ToLower(ext(f.File))]
		src, err := os.ReadFile(f.File)
		if err != nil {
			continue
		}
		lang := spec.lang()
		tree, err := ts.NewParser(lang).Parse(src)
		if err != nil {
			continue
		}
		// Re-locate the key node at the finding's position.
		key := tree.RootNode().NamedNodeAtByte(byteAt(src, f.Line, f.Col))
		if key == nil {
			continue
		}
		sp, ok := removalSpan(key, spec, lang, src)
		if !ok {
			continue
		}
		// Companion fork: eligible removals become replacements. Eligible =
		// the companioned top-level param, on a resource that accepts the
		// companion, in a file that doesn't already set it (spike-level
		// idempotence: one check per file, both spellings).
		if dec != nil && dec.insert && rule.Companion != nil &&
			f.Param == rule.Companion.ForParam &&
			!strings.Contains(string(src), rule.Companion.Param) &&
			!strings.Contains(string(src), pascal(rule.Companion.Param)) {
			if res := companionResourceFor(f.Anchor, rule.Companion.Resources); res != "" {
				jkey := f.File + "|" + sp.site + "|" + sp.receiver
				if sp.site != "statement" && sp.site != "chain-link" || !javaSeen[jkey] {
					if text, ok := companionText(spec, sp.site, res, sp.receiver); ok {
						sp.replace = spliceFor(sp.label, text)
						sp.label += "→+" + rule.Companion.Param
						javaSeen[jkey] = true
					}
				}
			}
		}
		if edits[f.File] == nil {
			edits[f.File] = &fileEdit{spec: spec}
		}
		edits[f.File].spans = append(edits[f.File].spans, sp)
	}

	var files []string
	for f := range edits {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, file := range files {
		fe := edits[file]
		src, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		// Apply spans back-to-front so offsets stay valid.
		sort.Slice(fe.spans, func(i, j int) bool { return fe.spans[i].start > fe.spans[j].start })
		out := src
		ff := FixFile{Path: file}
		for _, sp := range fe.spans {
			if sp.replace == "" {
				// Pure removal: widen to the whole line when only whitespace
				// would remain, so no blank line marks the spot.
				sp = expandToLine(sp, src)
			}
			if int(sp.end) > len(out) || sp.start >= sp.end {
				continue
			}
			spliced := append(append([]byte{}, out[:sp.start]...), sp.replace...)
			out = append(spliced, out[sp.end:]...)
			ff.BytesRemoved += int(sp.end - sp.start)
			ff.BytesAdded += len(sp.replace)
			if sp.replace != "" && report.Companion != nil {
				report.Companion.Inserts++
			}
			ff.Edits = append(ff.Edits, FixEdit{Start: sp.start, End: sp.end, Label: sp.label})
		}
		lang := fe.spec.lang()
		tree, err := ts.NewParser(lang).Parse(out)
		if err != nil || tree.RootNode().HasError() {
			ff.Reparse = "error"
			report.AllClean = false
		} else {
			ff.Reparse = "clean"
			if apply {
				info, statErr := os.Stat(file)
				mode := os.FileMode(0o644)
				if statErr == nil {
					mode = info.Mode()
				}
				if werr := os.WriteFile(file, out, mode); werr == nil {
					ff.Written = true
				} else {
					// Record and continue: a half-applied tree must still
					// produce a complete report of what happened.
					ff.Error = werr.Error()
					report.AllClean = false
				}
			}
		}
		report.Files = append(report.Files, ff)
	}
	return report, nil
}

func ext(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i:]
	}
	return ""
}

// byteAt converts a 1-based line:col back to a byte offset.
func byteAt(src []byte, line, col int) uint32 {
	l := 1
	for i := 0; i < len(src); i++ {
		if l == line {
			return uint32(i + col - 1)
		}
		if src[i] == '\n' {
			l++
		}
	}
	return 0
}
