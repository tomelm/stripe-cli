package main

// doctor.go — the account-aware layer that turns the scan's inventory into
// judgment. The scanner alone cannot say whether removing payment_method_types
// is safe; that depends on account facts. This layer fetches three of them with
// read-only GETs using the same credentials the CLI already stores:
//
//   1. /v1/account                          — who we're diagnosing
//   2. /v1/payment_method_configurations    — is the Dashboard actually configured
//   3. /v1/events?limit=N                   — which API versions traffic really
//                                             runs at (better than the account
//                                             default: it's the effective truth)
//
// No key is ever printed. Absent credentials degrade to scan-only.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// dpmCutoff is the API version at/after which removing payment_method_types
// alone enables dynamic payment methods; before it, automatic_payment_methods
// [enabled]=true must be added or methods are silently lost.
const dpmCutoff = "2023-08-16"

type accountFacts struct {
	AccountID   string
	DisplayName string

	// EventVersions maps api_version -> count over the sampled events.
	EventVersions map[string]int
	OldestVersion string

	// PMConfigs summarizes /v1/payment_method_configurations.
	ConfigCount    int
	ActiveConfig   string
	MethodsOn      int
	MethodsOff     int
	EnabledMethods []string // method names ON in the chosen config
	ConfiguredOK   bool
	NoEvents       bool // no recent events: version facts are unknowable
	VersionsOK     bool // every sampled event version >= dpmCutoff
	VersionsMixed  bool // some but not all versions >= dpmCutoff
}

// loadTestKey resolves a test-mode key without ever exposing it: env var
// first (what the CLI itself honors), then the CLI's own config.toml.
func loadTestKey(profile string) (string, error) {
	if k := os.Getenv("STRIPE_API_KEY"); k != "" {
		if !strings.HasPrefix(k, "sk_test_") && !strings.HasPrefix(k, "rk_test_") {
			return "", fmt.Errorf("STRIPE_API_KEY is not a test-mode key (sk_test_/rk_test_); refusing — this tool creates test objects")
		}
		return k, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var cfg map[string]map[string]any
	if _, err := toml.DecodeFile(filepath.Join(home, ".config", "stripe", "config.toml"), &cfg); err != nil {
		return "", fmt.Errorf("no STRIPE_API_KEY and could not read CLI config: %w", err)
	}
	p, ok := cfg[profile]
	if !ok {
		return "", fmt.Errorf("profile %q not found in CLI config", profile)
	}
	if k, ok := p["test_mode_api_key"].(string); ok && k != "" {
		return k, nil
	}
	return "", fmt.Errorf("profile %q has no test_mode_api_key", profile)
}

func stripeGET(key, path string, out any) error {
	req, err := http.NewRequest("GET", "https://api.stripe.com"+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(key, "")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return fmt.Errorf("credentials rejected (401) — key may be expired; run `stripe login`")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fetchAccountFacts(key string) (*accountFacts, error) {
	f := &accountFacts{EventVersions: map[string]int{}}

	var acct struct {
		ID       string `json:"id"`
		Settings struct {
			Dashboard struct {
				DisplayName string `json:"display_name"`
			} `json:"dashboard"`
		} `json:"settings"`
	}
	if err := stripeGET(key, "/v1/account", &acct); err != nil {
		return nil, err
	}
	f.AccountID, f.DisplayName = acct.ID, acct.Settings.Dashboard.DisplayName

	// Payment method configurations: each payment-method field is an object
	// with display_preference.value on|off. Parse generically.
	var pmc struct {
		Data []map[string]any `json:"data"`
	}
	if err := stripeGET(key, "/v1/payment_method_configurations", &pmc); err != nil {
		return nil, err
	}
	// Choose ONE governing config — the default if present, else the first
	// active — and count/collect methods from it alone (summing across
	// configs double-counted before).
	var chosen map[string]any
	for _, cfg := range pmc.Data {
		active, _ := cfg["active"].(bool)
		if !active {
			continue // deactivated demo leftovers shouldn't inflate anything
		}
		f.ConfigCount++
		isDefault, _ := cfg["is_default"].(bool)
		if isDefault {
			chosen = cfg
		} else if chosen == nil {
			chosen = cfg
		}
	}
	if chosen != nil {
		if name, ok := chosen["name"].(string); ok {
			f.ActiveConfig = name
		}
		for field, v := range chosen {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			dp, ok := m["display_preference"].(map[string]any)
			if !ok {
				continue
			}
			switch dp["value"] {
			case "on":
				f.MethodsOn++
				f.EnabledMethods = append(f.EnabledMethods, field)
			case "off":
				f.MethodsOff++
			}
		}
		sort.Strings(f.EnabledMethods)
	}
	f.ConfiguredOK = f.ConfigCount > 0 && f.MethodsOn > 0

	// Recent events: the API versions live traffic actually uses.
	var evts struct {
		Data []struct {
			APIVersion string `json:"api_version"`
		} `json:"data"`
	}
	if err := stripeGET(key, "/v1/events?limit=20", &evts); err != nil {
		return nil, err
	}
	ge := 0
	for _, e := range evts.Data {
		if e.APIVersion == "" {
			continue
		}
		f.EventVersions[e.APIVersion]++
		if f.OldestVersion == "" || datePrefix(e.APIVersion) < datePrefix(f.OldestVersion) {
			f.OldestVersion = e.APIVersion
		}
		if datePrefix(e.APIVersion) >= dpmCutoff {
			ge++
		}
	}
	total := 0
	for _, n := range f.EventVersions {
		total += n
	}
	f.NoEvents = total == 0
	f.VersionsOK = total > 0 && ge == total
	f.VersionsMixed = ge > 0 && ge < total
	return f, nil
}

// datePrefix compares versions by their YYYY-MM-DD prefix, which handles both
// classic ("2023-08-16") and named ("2026-06-24.dahlia") formats.
func datePrefix(v string) string {
	if len(v) >= 10 {
		return v[:10]
	}
	return v
}

// ---------- intent classification ----------

var identRe = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]*`)

// classifyIntent inspects a finding's value expression and buckets it:
//
//	default-shaped — static list of only card/link: the classic pre-DPM
//	                 hardcode and the migration's actual target
//	deliberate     — static single non-card method (['oxxo']): per-method
//	                 integration, almost certainly intentional
//	static         — static multi-method list beyond card/link
//	dynamic        — computed at runtime (identifiers, conditionals)
func classifyIntent(value string) string {
	v := strings.TrimSpace(value)
	quoted := regexp.MustCompile(`["']([a-z0-9_]+)["']`).FindAllStringSubmatch(v, -1)
	stripped := regexp.MustCompile(`["'][a-z0-9_]*["']|stripe\.StringSlice|\[\]string|Arrays\.asList|List\.of|[\[\]{},()\s]|new|List|string`).ReplaceAllString(v, "")
	if identRe.MatchString(stripped) || strings.ContainsAny(v, "?:") {
		return "dynamic"
	}
	if len(quoted) == 0 {
		return "dynamic"
	}
	onlyCardLink := true
	for _, q := range quoted {
		if q[1] != "card" && q[1] != "link" {
			onlyCardLink = false
		}
	}
	if onlyCardLink {
		return "default-shaped"
	}
	if len(quoted) == 1 {
		return "deliberate"
	}
	return "static"
}

// quotedMethods extracts the static method names from a finding's value.
func quotedMethods(value string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`["']([a-z0-9_]+)["']`).FindAllStringSubmatch(value, -1) {
		out = append(out, m[1])
	}
	return out
}

// verdict combines one finding's intent and value with the account facts.
func verdict(intent, value string, f *accountFacts) string {
	if f.NoEvents {
		// No traffic sampled: fabricating a version claim would be worse
		// than admitting ignorance.
		return "REVIEW: no recent API traffic to infer the account's effective version — confirm it is >= " + dpmCutoff + " before removing"
	}
	if !f.VersionsOK {
		if f.VersionsMixed {
			return "CAUTION: some recent traffic predates " + dpmCutoff + " — removal there silently drops methods unless automatic_payment_methods[enabled]=true is added"
		}
		return "BLOCKED: recent traffic runs before " + dpmCutoff + " — do not remove without adding automatic_payment_methods[enabled]=true"
	}
	if !f.ConfiguredOK {
		return "BLOCKED: no active Dashboard payment-method configuration — configure methods before removing"
	}
	// The doc's loudest warning: migration only keeps methods the Dashboard
	// has ON. Diff the hardcoded list against the governing config.
	if intent == "default-shaped" || intent == "static" {
		var missing []string
		enabled := map[string]bool{}
		for _, m := range f.EnabledMethods {
			enabled[m] = true
		}
		for _, m := range quotedMethods(value) {
			if !enabled[m] {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			return "CAUTION: " + strings.Join(missing, ", ") + " hardcoded here but OFF in the Dashboard config — enable in the Dashboard first or customers lose them on removal"
		}
	}
	switch intent {
	case "default-shaped":
		return "CANDIDATE: static card/link list, account preconditions met — removal enables Dashboard-managed methods (verify frontend is Payment Element)"
	case "deliberate":
		return "SKIP: single-method integration, restriction looks intentional — consider excluded_payment_method_types instead"
	case "dynamic":
		return "REVIEW: value computed at runtime — understand the routing logic before changing"
	default:
		return "REVIEW: static multi-method list — confirm whether the restriction is still wanted"
	}
}

// buildDoctorReport combines scan findings with account facts. The rule
// decides the judgment style: the dpm pack gets full account verdicts;
// advise packs get ADVISE verdicts carrying the rule's remediation message
// (their account precondition is the version window, shown as context).
func buildDoctorReport(findings []Finding, profile string, rule Rule) (*DoctorReport, error) {
	key, err := loadTestKey(profile)
	if err != nil {
		return nil, err
	}
	facts, err := fetchAccountFacts(key)
	if err != nil {
		return nil, err
	}

	r := &DoctorReport{
		Command: "doctor",
		Account: AccountSummary{
			ID:             facts.AccountID,
			Name:           facts.DisplayName,
			EventVersions:  facts.EventVersions,
			VersionsOK:     facts.VersionsOK,
			VersionsMixed:  facts.VersionsMixed,
			Cutoff:         dpmCutoff,
			Configs:        facts.ConfigCount,
			ActiveConfig:   facts.ActiveConfig,
			MethodsOn:      facts.MethodsOn,
			MethodsOff:     facts.MethodsOff,
			EnabledMethods: facts.EnabledMethods,
			NoRecentEvents: facts.NoEvents,
			ConfiguredOK:   facts.ConfiguredOK,
		},
		Summary: map[string]int{},
	}
	for _, f := range findings {
		intent := classifyIntent(f.Value)
		var v string
		if rule.Action == "advise" {
			v = "ADVISE: " + rule.Message
			if rule.IntroducedIn != "" {
				v += " (API " + rule.IntroducedIn + ")"
			}
		} else {
			v = verdict(intent, f.Value, facts)
		}
		class := v
		if i := strings.Index(v, ":"); i > 0 {
			class = v[:i]
		}
		r.Findings = append(r.Findings, DoctorFinding{Finding: f, Intent: intent, Verdict: v, Class: class})
		r.Summary[class]++
	}
	return r, nil
}

// ---------- code signals (substring-level, no credentials needed) ----------

// PackSignals is the pack-declared signal set: which webhook event types the
// migrated integration is expected to handle, which legacy client-side tokens
// indicate from-state code, and which package version floors apply.
type PackSignals struct {
	WebhookEvents  []string
	FrontendTokens []FrontendToken
	ManifestFloors []ManifestFloor
}

type FrontendToken struct {
	Token string
	Note  string
}

type ManifestFloor struct {
	Package string
	Min     string // "8.0.0"
}

// scanSignals walks the scanned directory for the pack's declared signals.
// These are honest substring/manifest checks, reported as signals — never
// verdicts.
func scanSignals(root string, sig *PackSignals) (*HandlerSignals, []FrontendWarning, []ManifestCheck) {
	if sig == nil {
		return nil, nil, nil
	}
	h := &HandlerSignals{}
	for _, ev := range sig.WebhookEvents {
		h.Events = append(h.Events, EventSignal{Event: ev})
	}
	var fw []FrontendWarning
	var mc []ManifestCheck

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", "vendor", ".git", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(path)
		if base == "package.json" && len(sig.ManifestFloors) > 0 {
			mc = append(mc, checkManifest(path, sig.ManifestFloors)...)
			return nil
		}
		if _, ok := specs[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(src)
		for i := range h.Events {
			if strings.Contains(text, h.Events[i].Event) {
				h.Events[i].Files = append(h.Events[i].Files, path)
				h.Events[i].Present = true
			}
		}
		for _, t := range sig.FrontendTokens {
			if strings.Contains(text, t.Token) {
				fw = append(fw, FrontendWarning{File: path, Signal: t.Token, Note: t.Note})
				break
			}
		}
		return nil
	})
	h.AllPresent = len(h.Events) > 0
	for _, e := range h.Events {
		if !e.Present {
			h.AllPresent = false
		}
	}
	return h, fw, mc
}

// checkManifest compares declared dependency versions against pack floors.
// Absent packages are OK (the floor applies only when the package is used).
func checkManifest(path string, floors []ManifestFloor) []ManifestCheck {
	var out []ManifestCheck
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return out
	}
	deps := map[string]string{}
	for k, v := range pkg.Dependencies {
		deps[k] = v
	}
	for k, v := range pkg.DevDependencies {
		deps[k] = v
	}
	for _, f := range floors {
		found, present := deps[f.Package]
		c := ManifestCheck{Package: f.Package, Floor: f.Min, Found: found, File: path, OK: true}
		if present {
			c.OK = versionAtLeast(found, f.Min)
		}
		out = append(out, c)
	}
	return out
}

// versionAtLeast does a lenient semver-ish compare: strips range prefixes and
// compares numeric dot segments. Unparseable versions count as OK (never
// false-alarm on what we can't read).
func versionAtLeast(have, want string) bool {
	clean := strings.TrimLeft(have, "^~>=v ")
	hp := strings.Split(clean, ".")
	wp := strings.Split(want, ".")
	for i := 0; i < len(wp); i++ {
		if i >= len(hp) {
			return false
		}
		var hn, wn int
		if _, err := fmt.Sscanf(strings.TrimSpace(hp[i]), "%d", &hn); err != nil {
			return true // unparseable -> no alarm
		}
		if _, err := fmt.Sscanf(wp[i], "%d", &wn); err != nil {
			return true
		}
		if hn != wn {
			return hn > wn
		}
	}
	return true
}
