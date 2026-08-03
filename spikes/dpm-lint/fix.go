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
	// replace, when non-empty, turns the deletion into a splice; variant
	// says which companion shape it is (plain | never-pin | return_url).
	replace string
	variant string
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
		fnText := nodeText(fn, src)
		if fn != nil {
			fnStart = fn.StartByte()
		} else {
			// Top-level code has no enclosing function; the whole file is
			// the scope (attribution checks are name-scoped, so this is
			// safe, and missing it hides top-level confirm mutations).
			fnText = string(src)
		}
		if p := inv.Parent(); p != nil && p.Type(lang) == "expression_statement" {
			receiver := nodeText(recv, src)
			return span{start: p.StartByte(), end: p.EndByte(), label: "statement", site: "statement",
				receiver: receiver,
				// Builder instance = receiver name scoped to its function.
				group:    fmt.Sprintf("stmt|%d|%s", fnStart, receiver),
				scope:    fnText,
				funcText: fnText}, true
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
			funcText: fnText}, true
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
	// literal) and this call's enclosing function; top-level code falls
	// back to the whole file (var-attribution checks are name-scoped).
	pairFunc := nodeText(climbFirst(pair, spec.funcKinds, lang), src)
	if pairFunc == "" {
		pairFunc = string(src)
	}
	base := span{site: site,
		scope:    nodeText(pair.Parent(), src),
		funcText: pairFunc}
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
		for _, line := range strings.Split(string(src), "\n") {
			// Only lines that name Stripe count: AWS/GitHub/Azure clients and
			// prose pin their own date-shaped API versions too.
			if !strings.Contains(strings.ToLower(line), "stripe") {
				continue
			}
			for _, m := range versionPinRe.FindAllStringSubmatch(line, -1) {
				v := m[2]
				if oldest == "" || v < oldest {
					oldest, at = v, path
				}
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

// siteConfirmsVia is scoped to THIS call site — the param bag for pair
// languages, the chain for chain links, the receiver's own statements for
// Java builders — PLUS, when the bag was resolved through a variable
// (via "var:<name>"), mutations of that variable in the enclosing function
// (params.confirm = true / params["confirm"] = True / params.Confirm =
// stripe.Bool(true)). It must NOT scan unrelated text: one confirming
// create must never pin allow_redirects onto a sibling create.
func siteConfirmsVia(sp span, via string) bool {
	switch sp.site {
	case "statement":
		return sp.receiver != "" && javaConfirmRe.MatchString(receiverStatements(sp))
	case "chain-link":
		return javaConfirmRe.MatchString(sp.scope)
	}
	if confirmTrueRe.MatchString(sp.scope) {
		return true
	}
	if name := varBagName(via); name != "" {
		return varMutationRe(name, "confirm").MatchString(sp.funcText)
	}
	return false
}

// receiverStatements extracts the enclosing function's statements that
// mention THIS builder receiver as a whole token (declaration or call), so
// sibling builders never contaminate the check — including builders whose
// name is a suffix of another (`b` vs `sb`), and calls whose arguments wrap
// across lines (statements are split on ';', not newlines).
func receiverStatements(sp span) string {
	re := regexp.MustCompile(`(^|[^\w$])` + regexp.QuoteMeta(sp.receiver) + `\s*[.=]`)
	var out []string
	for _, stmt := range strings.Split(sp.funcText, ";") {
		if re.MatchString(stmt) {
			out = append(out, stmt)
		}
	}
	return strings.Join(out, ";")
}

// varBagName extracts the variable name from a "var:<name>" resolution.
func varBagName(via string) string {
	if strings.HasPrefix(via, "var:") {
		return strings.TrimPrefix(via, "var:")
	}
	return ""
}

// varMutationRe matches `<name>.<param> = true`-shaped mutations across the
// pair SDK spellings: params.confirm = true, params["confirm"] = True,
// params.Confirm = stripe.Bool(true), params[:confirm] = true.
func varMutationRe(name, param string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^\w$])` + regexp.QuoteMeta(name) +
		`(\.|\[)["':]?` + param + `["']?\]?\s*(=|:)\s*(stripe\.Bool\()?\s*true`)
}

// varAssignRe matches any assignment of <param> onto the variable (used for
// return_url presence, where the assigned value is a string, not a bool).
func varAssignRe(name, param string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^\w$])` + regexp.QuoteMeta(name) +
		`(\.|\[)["':]?` + param + `["']?\]?\s*=`)
}

// siteHasReturnURLVia is deliberately NARROW — the bag/chain, this
// receiver's own statements for Java, or this bag-variable's own mutations:
// claiming a return_url exists when it doesn't yields the runtime 400 this
// logic prevents; the reverse merely pins allow_redirects conservatively.
func siteHasReturnURLVia(sp span, via string) bool {
	text := sp.scope
	if sp.site == "statement" {
		text = receiverStatements(sp)
	}
	for _, tok := range []string{"return_url", "ReturnURL", "ReturnUrl", "setReturnUrl"} {
		if strings.Contains(text, tok) {
			return true
		}
	}
	if name := varBagName(via); name != "" {
		if varAssignRe(name, "return_url").MatchString(sp.funcText) ||
			varAssignRe(name, "ReturnURL").MatchString(sp.funcText) {
			return true
		}
	}
	return false
}

// nonRedirectMethods is a SAFELIST of methods documented to complete
// without leaving the page (cards, bank debits, vouchers, balance, Link's
// inline flow). Anything NOT listed — including methods added to the API
// after this list was written — is treated as redirect-capable, so unknown
// methods land on the conservative branch (gate, not pin).
var nonRedirectMethods = map[string]bool{
	"card": true, "card_present": true, "interac_present": true,
	"us_bank_account": true, "sepa_debit": true, "bacs_debit": true,
	"au_becs_debit": true, "acss_debit": true, "nz_bank_account": true,
	"boleto": true, "oxxo": true, "konbini": true, "multibanco": true,
	"customer_balance": true, "link": true,
}

// methodsNeedingRedirect returns the methods in a removed value that a
// confirm-site pin would silently disable.
func methodsNeedingRedirect(value string) []string {
	var hits []string
	for _, m := range quotedMethods(value) {
		if !nonRedirectMethods[m] {
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
	// Pass 1: locate every finding's span (sources and trees cached per file).
	type siteRec struct {
		f    Finding
		spec langSpec
		sp   span
	}
	srcCache := map[string][]byte{}
	var recs []siteRec
	for _, f := range findings {
		spec := specs[strings.ToLower(ext(f.File))]
		src, ok := srcCache[f.File]
		if !ok {
			b, rerr := os.ReadFile(f.File)
			if rerr != nil {
				continue
			}
			src = b
			srcCache[f.File] = b
		}
		lang := spec.lang()
		tree, terr := ts.NewParser(lang).Parse(src)
		if terr != nil {
			continue
		}
		key := tree.RootNode().NamedNodeAtByte(byteAt(src, f.Line, f.Col))
		if key == nil {
			continue
		}
		sp, ok := removalSpan(key, spec, lang, src)
		if !ok {
			continue
		}
		recs = append(recs, siteRec{f: f, spec: spec, sp: sp})
	}

	// Pass 2: group records into CALL SITES. A pair-language finding is its
	// own site; Java splits one builder's method list across N findings (one
	// per addPaymentMethodType), which MUST be judged as a unit — a gate
	// that skips one link but removes another would leave half a parameter
	// next to an inserted companion, code the API rejects outright.
	type callSite struct{ recs []siteRec }
	var sites []*callSite
	byGroup := map[string]*callSite{}
	for _, r := range recs {
		if r.sp.group == "" {
			sites = append(sites, &callSite{recs: []siteRec{r}})
			continue
		}
		k := r.f.File + "|" + r.sp.group
		if byGroup[k] == nil {
			byGroup[k] = &callSite{}
			sites = append(sites, byGroup[k])
		}
		byGroup[k].recs = append(byGroup[k].recs, r)
	}

	// Pass 3: one decision per site — intent gate, confirm handling,
	// companion variant — then emit all of the site's spans together.
	for _, st := range sites {
		lead := st.recs[0]
		combined := lead.f.Value
		if len(st.recs) > 1 {
			var vs []string
			for _, r := range st.recs {
				vs = append(vs, r.f.Value)
			}
			combined = strings.Join(vs, ", ")
		}
		// Intent at SITE granularity: a builder adding card+ideal is a
		// static multi-method list, not a deliberate single restriction.
		intent := classifyIntent(lead.f.Value)
		if len(st.recs) > 1 {
			intent = "static"
			for _, r := range st.recs {
				if classifyIntent(r.f.Value) == "dynamic" {
					intent = "dynamic"
					break
				}
			}
		}
		if !includeAll && (intent == "dynamic" || intent == "deliberate") {
			reason := "value is computed at runtime — review the routing logic (use --all to override)"
			if intent == "deliberate" {
				reason = "single-method restriction looks intentional — consider excluded_payment_method_types (use --all to override; note wallet-class methods use the wallets hash instead)"
			}
			report.Skipped = append(report.Skipped, SkippedFinding{File: lead.f.File, Line: lead.f.Line, Intent: intent, Reason: reason})
			continue
		}

		// Companion handling. Server-side-confirmation sites (confirm:true,
		// no return_url) are evaluated on BOTH branches of the version fork:
		// after removal, automatic_payment_methods is in effect either way
		// (inserted below the cutoff, default-on above it), so the runtime
		// 400 exists on both. Per confirm site:
		//   --return-url given          → companion + return_url
		//   list had redirect-capable
		//   methods, no return_url      → GATE (pinning drops methods the
		//                                 merchant used; removal alone 400s)
		//   otherwise                   → companion + allow_redirects:"never"
		var replaceText, variant string
		if dec != nil && rule.Companion != nil && lead.f.Param == rule.Companion.ForParam &&
			!alreadyHasCompanion(lead.sp, rule.Companion.Param) {
			if res := companionResourceFor(lead.spec, lead.f.Anchor, lead.sp.funcText, rule.Companion.Resources); res != "" {
				confirmSite := siteConfirmsVia(lead.sp, lead.f.Via) && !siteHasReturnURLVia(lead.sp, lead.f.Via)
				if confirmSite && dec.returnURL == "" {
					if rm := methodsNeedingRedirect(combined); len(rm) > 0 {
						report.Skipped = append(report.Skipped, SkippedFinding{File: lead.f.File, Line: lead.f.Line, Intent: "confirm-redirect",
							Reason: "server-side confirmation uses redirect-capable method(s) " + strings.Join(rm, ", ") +
								" — removal would either fail at runtime (no return_url) or drop them (allow_redirects:\"never\"); rerun with --return-url <url>"})
						continue
					}
				}
				// Confirm sites force the explicit insert even on the omit
				// branch — that is where the pin/return_url must live.
				if dec.insert || confirmSite {
					var opts companionOpts
					if confirmSite {
						if dec.returnURL != "" {
							opts.returnURL = dec.returnURL
							variant = "return_url"
						} else {
							opts.allowRedirectsNever = true
							variant = "never-pin"
						}
					}
					if text, ok := companionText(lead.spec, lead.sp.site, res, lead.sp.receiver, opts); ok {
						replaceText = spliceFor(lead.sp.label, text)
						if variant == "" {
							variant = "plain"
						}
					}
				}
			}
		}

		for i, r := range st.recs {
			sp := r.sp
			if i == 0 && replaceText != "" {
				sp.replace = replaceText
				sp.variant = variant
				label := "→+" + rule.Companion.Param
				switch variant {
				case "never-pin":
					label += `(allow_redirects:"never")`
					confirmNever++
					if report.Companion != nil {
						report.Companion.PinnedSites = append(report.Companion.PinnedSites, SiteRef{File: r.f.File, Line: r.f.Line})
					}
				case "return_url":
					label += "+return_url"
					confirmWithURL++
				}
				sp.label += label
			}
			if edits[r.f.File] == nil {
				edits[r.f.File] = &fileEdit{spec: r.spec}
			}
			edits[r.f.File].spans = append(edits[r.f.File].spans, sp)
		}
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
			ff.Edits = append(ff.Edits, FixEdit{Start: sp.start, End: sp.end, Label: sp.label, Variant: sp.variant})
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
		if report.Companion.Mode == "omit" && (confirmNever > 0 || confirmWithURL > 0) {
			report.Companion.Notes = append(report.Companion.Notes,
				"confirm site(s) received an explicit companion despite the omit verdict: automatic_payment_methods is default-on at this account's version, so server-side confirmation still needs the pin or a return_url to avoid a runtime 400")
		}
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
