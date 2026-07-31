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
	ConfigCount   int
	ActiveConfig  string
	MethodsOn     int
	MethodsOff    int
	ConfiguredOK  bool
	VersionsOK    bool // every sampled event version >= dpmCutoff
	VersionsMixed bool // some but not all versions >= dpmCutoff
}

// loadTestKey resolves a test-mode key without ever exposing it: env var
// first (what the CLI itself honors), then the CLI's own config.toml.
func loadTestKey(profile string) (string, error) {
	if k := os.Getenv("STRIPE_API_KEY"); k != "" {
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
	f.ConfigCount = len(pmc.Data)
	for _, cfg := range pmc.Data {
		active, _ := cfg["active"].(bool)
		isDefault, _ := cfg["is_default"].(bool)
		if !active || (!isDefault && f.ActiveConfig != "") {
			continue
		}
		if name, ok := cfg["name"].(string); ok {
			f.ActiveConfig = name
		}
		for field, v := range cfg {
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
			case "off":
				f.MethodsOff++
			}
			_ = field
		}
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

// verdict combines one finding's intent with the account facts.
func verdict(intent string, f *accountFacts) string {
	if !f.VersionsOK {
		if f.VersionsMixed {
			return "CAUTION: some recent traffic predates " + dpmCutoff + " — removal there silently drops methods unless automatic_payment_methods[enabled]=true is added"
		}
		return "BLOCKED: recent traffic runs before " + dpmCutoff + " — do not remove without adding automatic_payment_methods[enabled]=true"
	}
	if !f.ConfiguredOK {
		return "BLOCKED: no active Dashboard payment-method configuration — configure methods before removing"
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

// runDoctor renders the combined report.
func runDoctor(findings []Finding, profile string) {
	key, err := loadTestKey(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\ndoctor: continuing scan-only (set STRIPE_API_KEY or run `stripe login`)\n", err)
		return
	}
	facts, err := fetchAccountFacts(key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\ndoctor: continuing scan-only\n", err)
		return
	}

	fmt.Printf("\nAccount %s", facts.AccountID)
	if facts.DisplayName != "" {
		fmt.Printf(" (%s)", facts.DisplayName)
	}
	fmt.Println(" — test mode")

	var vs []string
	for v, n := range facts.EventVersions {
		vs = append(vs, fmt.Sprintf("%s ×%d", v, n))
	}
	sort.Strings(vs)
	if len(vs) == 0 {
		fmt.Println("  recent event API versions: none sampled")
	} else {
		fmt.Printf("  recent event API versions: %s\n", strings.Join(vs, ", "))
	}
	fmt.Printf("  DPM cutoff %s: versionsOK=%v mixed=%v\n", dpmCutoff, facts.VersionsOK, facts.VersionsMixed)
	fmt.Printf("  payment-method configurations: %d (active: %q) — %d methods on, %d off\n",
		facts.ConfigCount, facts.ActiveConfig, facts.MethodsOn, facts.MethodsOff)

	if len(findings) == 0 {
		fmt.Println("\nNo hardcoded payment_method_types found — nothing to migrate.")
		return
	}
	fmt.Printf("\nVerdicts (%d findings)\n", len(findings))
	for _, f := range findings {
		intent := classifyIntent(f.Value)
		fmt.Printf("  %s:%d  [%s] %s\n      %s\n", f.File, f.Line, intent, firstLine(f.Value), verdict(intent, facts))
	}
}
