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
	"path/filepath"
	"regexp"
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
	// group identifies the builder INSTANCE for Java dedupe: same-named
	// builders in different functions, and distinct chains in one file, must
	// not collapse onto each other.
	group string
	// scope is the text checked for an already-present companion: the param
	// bag for pair languages, the whole chain for chain links, the enclosing
	// function for builder statements.
	scope string
	// funcText is the enclosing function's text, for create-evidence checks
	// that the anchor alone cannot answer (Go's shared params struct).
	funcText string
	// replace, when non-empty, turns the deletion into a splice.
	replace string
}

// climbLast walks ancestors of n and returns the OUTERMOST node whose kind is
// in kinds (nil when none).
func climbLast(n *ts.Node, kinds []string, lang *ts.Language) *ts.Node {
	var last *ts.Node
	for cur := n.Parent(); cur != nil; cur = cur.Parent() {
		if containsStr(kinds, cur.Type(lang)) {
			last = cur
		}
	}
	return last
}

// climbFirst returns the NEAREST ancestor of n whose kind is in kinds.
func climbFirst(n *ts.Node, kinds []string, lang *ts.Language) *ts.Node {
	for cur := n.Parent(); cur != nil; cur = cur.Parent() {
		if containsStr(kinds, cur.Type(lang)) {
			return cur
		}
	}
	return nil
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
		fn := climbFirst(inv, spec.funcKinds, lang)
		fnStart := uint32(0)
		if fn != nil {
			fnStart = fn.StartByte()
		}
		if p := inv.Parent(); p != nil && p.Type(lang) == "expression_statement" {
			receiver := nodeText(recv, src)
			return span{start: p.StartByte(), end: p.EndByte(), label: "statement", site: "statement",
				receiver: receiver,
				// Builder instance = receiver name scoped to its function.
				group:    fmt.Sprintf("stmt|%d|%s", fnStart, receiver),
				scope:    nodeText(fn, src),
				funcText: nodeText(fn, src)}, true
		}
		if recv == nil {
			return span{}, false
		}
		// Every link of one chain shares the same OUTERMOST invocation node,
		// which is exactly the builder-instance identity we need.
		outer := climbLast(key, spec.anchorKinds, lang)
		outerStart := inv.StartByte()
		if outer != nil {
			outerStart = outer.StartByte()
		}
		return span{start: recv.EndByte(), end: inv.EndByte(), label: "chain-link", site: "chain-link",
			group:    fmt.Sprintf("chain|%d", outerStart),
			scope:    nodeText(outer, src),
			funcText: nodeText(fn, src)}, true
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
	// The companion checks are scoped to THIS param bag (the pair's parent
	// literal) and this call's enclosing function — never the whole file.
	base := span{site: site,
		scope:    nodeText(pair.Parent(), src),
		funcText: nodeText(climbFirst(pair, spec.funcKinds, lang), src)}
	s, e := pair.StartByte(), pair.EndByte()
	// Prefer swallowing the trailing comma; else the leading one.
	if i := skipWS(src, int(e), +1); i < len(src) && src[i] == ',' {
		base.start, base.end, base.label = s, uint32(i+1), "pair+trailing-comma"
		return base, true
	}
	if i := skipWS(src, int(s)-1, -1); i >= 0 && src[i] == ',' {
		base.start, base.end, base.label = uint32(i), e, "pair+leading-comma"
		return base, true
	}
	base.start, base.end, base.label = s, e, "pair"
	return base, true
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
	insert  bool
	reason  string
	oldest  string
	account string
	// returnURL, when set (--return-url), is inserted alongside the companion
	// at server-side-confirmation sites so redirect-based payment methods
	// keep working; without it those sites get allow_redirects:"never".
	returnURL string
}

// versionPinRe finds an API version pinned in code near a version-ish
// keyword: apiVersion: '2022-11-15', stripe.api_version = "...",
// .setApiVersion("..."), Stripe-Version headers. Named suffixes
// (.dahlia) compare by date prefix.
var versionPinRe = regexp.MustCompile(`(?i)(stripe[-_ ]?version|api[-_ ]?version)[^\n]{0,40}?(20\d\d-\d\d-\d\d)`)

// scanVersionPins walks the scan tree for pinned API versions in code —
// stronger evidence than the events census, which only reflects the ACCOUNT
// DEFAULT version (an SDK pinning an older Stripe-Version runs below it
// invisibly). Returns the oldest pin and where it was found.
func scanVersionPins(root string) (oldest, at string) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "vendor", ".git", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := specs[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range versionPinRe.FindAllSubmatch(src, -1) {
			v := string(m[2])
			if oldest == "" || v < oldest {
				oldest, at = v, path
			}
		}
		return nil
	})
	return oldest, at
}

// resolveCompanion decides the fork from account facts plus code evidence.
// Every unknowable branch (offline, no credentials, no recent events)
// resolves to INSERT: the companion is semantically a no-op at/after the
// cutoff and load-bearing below it, so inserting is the only choice that is
// correct at any version. A version pinned in CODE below the cutoff
// overrides an omit verdict — the census only sees the account default.
func resolveCompanion(rule Rule, profile, stripeAccount, root string, offline bool) *companionDecision {
	unknown := func(why string) *companionDecision {
		return &companionDecision{insert: true,
			reason: why + " — traffic API versions unknown; inserting " + rule.Companion.Param + " (a no-op at/after " + rule.IntroducedIn + ", required below it)"}
	}
	dec := func() *companionDecision {
		if offline {
			return unknown("--offline")
		}
		key, err := loadTestKey(profile)
		if err != nil {
			return unknown("no credentials (" + err.Error() + ")")
		}
		facts, err := fetchAccountFacts(key, stripeAccount)
		if err != nil {
			return unknown("account lookup failed (" + err.Error() + ")")
		}
		d := decideCompanion(rule, facts)
		d.account = facts.AccountID
		return d
	}()
	if pin, at := scanVersionPins(root); pin != "" && datePrefix(pin) < rule.IntroducedIn {
		if !dec.insert {
			dec.insert = true
			dec.reason = "code pins Stripe-Version " + pin + " (" + at + ") — below " + rule.IntroducedIn + "; the events census only reflects the account default, so the pin wins and " + rule.Companion.Param + " is inserted"
		} else {
			dec.reason += "; code also pins " + pin + " (" + at + ")"
		}
	}
	return dec
}

// decideCompanion is the pure fork: it recomputes the version census against
// THIS rule's cutoff (accountFacts' own VersionsOK is bound to the dpm
// constant, which only coincidentally matches) and folds in the Dashboard-
// configuration caveat the doctor would raise on the same facts.
func decideCompanion(rule Rule, facts *accountFacts) *companionDecision {
	cutoff := rule.IntroducedIn
	total, atOrAfter := 0, 0
	for v, n := range facts.EventVersions {
		total += n
		if datePrefix(v) >= cutoff {
			atOrAfter += n
		}
	}
	configNote := ""
	if !facts.ConfiguredOK {
		configNote = "; NOTE: no active Dashboard payment-method configuration — doctor reports BLOCKED until methods are configured"
	}
	switch {
	case total == 0:
		return &companionDecision{insert: true,
			reason: "no recent events — traffic API versions unknown; inserting " + rule.Companion.Param + " (a no-op at/after " + cutoff + ", required below it)" + configNote}
	case atOrAfter == total:
		return &companionDecision{insert: false, oldest: facts.OldestVersion,
			reason: "all recent traffic runs at/after " + cutoff + " — " + rule.Companion.Param + " is default-enabled there; plain removal is behavior-preserving" + configNote}
	default:
		return &companionDecision{insert: true, oldest: facts.OldestVersion,
			reason: "recent traffic runs below " + cutoff + " (oldest " + facts.OldestVersion + ") — bare removal would silently drop methods; inserting " + rule.Companion.Param + configNote}
	}
}

// companionResourceFor maps a removal site onto one of the companion's
// eligible API resources ("" = not eligible). Eligibility needs CREATE
// evidence, not just the resource name: automatic_payment_methods is a
// create-only parameter, so update/confirm/modify sites must never gain it
// (the API rejects it there), and a Checkout Session builder that merely
// mentions PaymentIntentData must not match payment_intents.
func companionResourceFor(spec langSpec, anchor, funcText string, resources []string) string {
	for _, r := range resources {
		single := pascalSingular(r)
		switch spec.name {
		case "java", "csharp":
			// The create params class is the token; Update/Confirm classes
			// are distinct and never match.
			if strings.Contains(anchor, single+"CreateParams") || strings.Contains(anchor, single+"CreateOptions") {
				return r
			}
		case "go":
			// stripe-go shares one params struct across create/update/confirm;
			// the verb lives at the call site. Require create evidence (.New)
			// and no update/confirm evidence in the enclosing function.
			if strings.Contains(anchor, single+"Params") &&
				strings.Contains(funcText, ".New(") &&
				!strings.Contains(funcText, ".Update(") && !strings.Contains(funcText, ".Confirm(") {
				return r
			}
		default:
			// Dynamic SDKs name the verb in the call itself.
			if (strings.Contains(anchor, single) || strings.Contains(anchor, lowerCamelPlural(r))) &&
				strings.Contains(strings.ToLower(anchor), "create") &&
				!strings.Contains(strings.ToLower(anchor), "update") &&
				!strings.Contains(strings.ToLower(anchor), "confirm") &&
				!strings.Contains(strings.ToLower(anchor), "modify") {
				return r
			}
		}
	}
	return ""
}

// alreadyHasCompanion reports whether the removal site's own scope (param
// bag / builder chain / function for builder statements) already sets the
// companion — scoped per call site, so a half-migrated file still gets the
// insert on its remaining calls.
func alreadyHasCompanion(sp span, param string) bool {
	switch sp.site {
	case "statement":
		return strings.Contains(sp.scope, sp.receiver+".set"+pascal(param))
	case "chain-link":
		return strings.Contains(sp.scope, ".set"+pascal(param))
	}
	return strings.Contains(sp.scope, param) || strings.Contains(sp.scope, pascal(param))
}

// companionOpts selects the companion variant for a site. Server-side
// confirmation (confirm:true) with automatic_payment_methods and no
// return_url is a 400 at runtime
// (payment_intent_automatic_payment_method_confirmation_allow_redirects_
// without_return_url): such sites must either gain a merchant-provided
// return_url or pin allow_redirects to "never".
type companionOpts struct {
	allowRedirectsNever bool
	returnURL           string
}

// confirmTrueRe matches confirm being set truthy across the pair-shaped
// SDKs (confirm: true / confirm=True / 'confirm' => true / Confirm =
// true / Confirm: stripe.Bool(true)). Both boundaries are guarded: a word
// character (or $) may not precede OR follow "confirm", so auditConfirm,
// confirmation_method and reconfirm never match.
var confirmTrueRe = regexp.MustCompile(`(?i)(^|[^\w$])["']?confirm["']?\s*(=>|=|:)\s*(stripe\.Bool\()?\s*true`)

// javaConfirmRe matches the builder spelling.
var javaConfirmRe = regexp.MustCompile(`setConfirm\(\s*true`)

// siteConfirms is scoped to THIS call site — the param bag for pair
// languages (Confirm lives in the same bag), the chain for chain links, and
// the receiver's own statements for Java builder statements. It must NOT
// scan the whole function: one confirming create must never pin
// allow_redirects onto a sibling create.
func siteConfirms(sp span) bool {
	switch sp.site {
	case "statement":
		return sp.receiver != "" && javaConfirmRe.MatchString(receiverCalls(sp))
	case "chain-link":
		return javaConfirmRe.MatchString(sp.scope)
	}
	return confirmTrueRe.MatchString(sp.scope)
}

// receiverCalls extracts only the lines of the enclosing function that
// invoke THIS builder receiver, so sibling builders in the same method never
// contaminate the check.
func receiverCalls(sp span) string {
	var out []string
	for _, line := range strings.Split(sp.funcText, "\n") {
		if strings.Contains(line, sp.receiver+".") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// siteHasReturnURL is deliberately NARROW — the bag/chain, or this
// receiver's own statements for Java: claiming a return_url exists when it
// doesn't yields the runtime 400 this logic prevents; the reverse merely
// pins allow_redirects conservatively.
func siteHasReturnURL(sp span) bool {
	text := sp.scope
	if sp.site == "statement" {
		text = receiverCalls(sp)
	}
	for _, tok := range []string{"return_url", "ReturnURL", "ReturnUrl", "setReturnUrl"} {
		if strings.Contains(text, tok) {
			return true
		}
	}
	return false
}

// redirectMethods are payment methods whose flows leave the page and come
// back — the ones allow_redirects:"never" would silently disable. Curated
// approximation, documented as such: when a confirm-site's removed hardcoded
// list names one of these and no return_url is available, the fix GATES the
// site instead of pinning, because pinning would drop a method the merchant
// was explicitly using.
var redirectMethods = map[string]bool{
	"ideal": true, "bancontact": true, "giropay": true, "sofort": true,
	"eps": true, "p24": true, "alipay": true, "wechat_pay": true,
	"klarna": true, "afterpay_clearpay": true, "affirm": true,
	"paypal": true, "twint": true, "mobilepay": true, "swish": true,
	"blik": true, "fpx": true, "grabpay": true, "kakao_pay": true,
	"naver_pay": true, "payco": true, "satispay": true,
}

func redirectMethodsIn(value string) []string {
	var hits []string
	for _, m := range quotedMethods(value) {
		if redirectMethods[m] {
			hits = append(hits, m)
		}
	}
	return hits
}

// companionText renders the language-correct insertion of
// automatic_payment_methods[enabled]=true for a removal site. site is the
// pair node kind (or statement/chain-link for Java builders); resource picks
// the typed SDKs' params-class prefix.
func companionText(spec langSpec, site, resource, receiver string, opts companionOpts) (string, bool) {
	prefix := pascalSingular(resource) // PaymentIntent | SetupIntent
	switch spec.name {
	case "ruby", "javascript", "typescript", "tsx":
		apm := "automatic_payment_methods: {enabled: true}"
		if opts.allowRedirectsNever {
			apm = "automatic_payment_methods: {enabled: true, allow_redirects: 'never'}"
		}
		if opts.returnURL != "" {
			apm += ", return_url: '" + opts.returnURL + "'"
		}
		return apm, true
	case "python":
		if site == "keyword_argument" {
			apm := `automatic_payment_methods={"enabled": True}`
			if opts.allowRedirectsNever {
				apm = `automatic_payment_methods={"enabled": True, "allow_redirects": "never"}`
			}
			if opts.returnURL != "" {
				apm += `, return_url="` + opts.returnURL + `"`
			}
			return apm, true
		}
		apm := `"automatic_payment_methods": {"enabled": True}`
		if opts.allowRedirectsNever {
			apm = `"automatic_payment_methods": {"enabled": True, "allow_redirects": "never"}`
		}
		if opts.returnURL != "" {
			apm += `, "return_url": "` + opts.returnURL + `"`
		}
		return apm, true
	case "php":
		apm := "'automatic_payment_methods' => ['enabled' => true]"
		if opts.allowRedirectsNever {
			apm = "'automatic_payment_methods' => ['enabled' => true, 'allow_redirects' => 'never']"
		}
		if opts.returnURL != "" {
			apm += ", 'return_url' => '" + opts.returnURL + "'"
		}
		return apm, true
	case "go":
		inner := "Enabled: stripe.Bool(true)"
		if opts.allowRedirectsNever {
			inner += `, AllowRedirects: stripe.String("never")`
		}
		apm := "AutomaticPaymentMethods: &stripe." + prefix + "AutomaticPaymentMethodsParams{" + inner + "}"
		if opts.returnURL != "" {
			apm += `, ReturnURL: stripe.String("` + opts.returnURL + `")`
		}
		return apm, true
	case "csharp":
		inner := "Enabled = true"
		if opts.allowRedirectsNever {
			inner += `, AllowRedirects = "never"`
		}
		apm := "AutomaticPaymentMethods = new " + prefix + "AutomaticPaymentMethodsOptions { " + inner + " }"
		if opts.returnURL != "" {
			apm += `, ReturnUrl = "` + opts.returnURL + `"`
		}
		return apm, true
	case "java":
		builder := prefix + "CreateParams.AutomaticPaymentMethods.builder().setEnabled(true)"
		if opts.allowRedirectsNever {
			builder += ".setAllowRedirects(" + prefix + "CreateParams.AutomaticPaymentMethods.AllowRedirects.NEVER)"
		}
		call := ".setAutomaticPaymentMethods(" + builder + ".build())"
		if opts.returnURL != "" {
			call += `.setReturnUrl("` + opts.returnURL + `")`
		}
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
	// Server-side-confirmation site counters for the companion notes.
	confirmNever, confirmWithURL := 0, 0

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
		report.Companion = &CompanionReport{Param: rule.Companion.Param, Mode: mode, Reason: dec.reason, OldestVersion: dec.oldest, Account: dec.account}
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
		// Companion handling. Server-side-confirmation sites (confirm:true,
		// no return_url) are evaluated on BOTH branches of the version fork:
		// after removal, automatic_payment_methods is in effect either way
		// (inserted below the cutoff, default-on above it), so the runtime
		// 400 exists on both. Three outcomes per confirm site:
		//   --return-url given      → companion + return_url (keeps redirects)
		//   old list had redirect
		//   methods, no return_url  → GATE the site (pinning would silently
		//                             drop a method the merchant was using;
		//                             bare removal would 400)
		//   otherwise               → companion + allow_redirects:"never"
		if dec != nil && rule.Companion != nil && f.Param == rule.Companion.ForParam &&
			!alreadyHasCompanion(sp, rule.Companion.Param) {
			res := companionResourceFor(spec, f.Anchor, sp.funcText, rule.Companion.Resources)
			confirmSite := res != "" && siteConfirms(sp) && !siteHasReturnURL(sp)
			if confirmSite && dec.returnURL == "" {
				if rm := redirectMethodsIn(f.Value); len(rm) > 0 {
					report.Skipped = append(report.Skipped, SkippedFinding{File: f.File, Line: f.Line, Intent: "confirm-redirect",
						Reason: "server-side confirmation uses redirect-based method(s) " + strings.Join(rm, ", ") +
							" — removal would either fail at runtime (no return_url) or drop them (allow_redirects:\"never\"); rerun with --return-url <url>"})
					continue
				}
			}
			// Confirm sites force the explicit insert even on the omit
			// branch — that is where the pin/return_url must live.
			if res != "" && (dec.insert || confirmSite) {
				jkey := f.File + "|" + sp.group
				if sp.group == "" || !javaSeen[jkey] {
					var opts companionOpts
					variant := ""
					if confirmSite {
						if dec.returnURL != "" {
							opts.returnURL = dec.returnURL
							variant = "+return_url"
						} else {
							opts.allowRedirectsNever = true
							variant = `(allow_redirects:"never")`
						}
					}
					if text, ok := companionText(spec, sp.site, res, sp.receiver, opts); ok {
						sp.replace = spliceFor(sp.label, text)
						sp.label += "→+" + rule.Companion.Param + variant
						javaSeen[jkey] = true
						switch {
						case opts.returnURL != "":
							confirmWithURL++
						case opts.allowRedirectsNever:
							confirmNever++
							if report.Companion != nil {
								report.Companion.PinnedSites = append(report.Companion.PinnedSites, SiteRef{File: f.File, Line: f.Line})
							}
						}
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
	if report.Companion != nil {
		if confirmNever > 0 {
			report.Companion.Notes = append(report.Companion.Notes, fmt.Sprintf(
				"%d server-side-confirmation site(s) got allow_redirects:\"never\" (no return_url present): APM+confirm without a return_url is a runtime 400; rerun with --return-url <url> to enable redirect-based payment methods instead", confirmNever))
		}
		if confirmWithURL > 0 {
			report.Companion.Notes = append(report.Companion.Notes, fmt.Sprintf(
				"%d server-side-confirmation site(s) gained the provided return_url alongside the companion", confirmWithURL))
		}
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
