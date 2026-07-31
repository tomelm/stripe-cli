package main

// flow.go — the docs-as-interactive-experience prototype. Executes the two
// dashboard-payment-methods doc variants as verifiable local steps:
//
//	A. Session-diff loop: create an EPHEMERAL payment-method configuration
//	   (the user's real Default config is never touched), create a Checkout
//	   Session against it, toggle one method off on the ephemeral config,
//	   create a second session, and diff the resolved payment_method_types.
//	   This demonstrates the doc's central claim — methods come from the
//	   Dashboard config, not code — as an observable local experiment.
//	B. Delayed-notification drill: start a local webhook handler (the doc's
//	   Ruby sample, in Go), run `stripe listen --forward-to` it, `stripe
//	   trigger checkout.session.async_payment_succeeded`, and verify the
//	   event arrives with a valid signature.
//	C. Embedded variant: serve a local page that mounts the embedded
//	   checkout (ui_mode=embedded_page) for browser verification.
//
// All test mode. The ephemeral config is deactivated at the end. Session
// creation and config writes are printed as they happen.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

func stripePOST(key, path string, form url.Values, out any) error {
	return stripePOSTv(key, path, "", form, out)
}

// stripePOSTv pins a Stripe-Version header when version is non-empty. Needed
// because ui_mode=embedded_page requires >= 2026-03-25.dahlia while the
// account default may be far older — the exact per-request override the
// migration docs describe.
func stripePOSTv(key, path, version string, form url.Values, out any) error {
	req, err := http.NewRequest("POST", "https://api.stripe.com"+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(key, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if version != "" {
		req.Header.Set("Stripe-Version", version)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return fmt.Errorf("POST %s: HTTP %d: %s", path, resp.StatusCode, e.Error.Message)
	}
	return json.Unmarshal(body, out)
}

type session struct {
	ID                 string   `json:"id"`
	URL                string   `json:"url"`
	ClientSecret       string   `json:"client_secret"`
	PaymentMethodTypes []string `json:"payment_method_types"`
}

func createSession(key, pmcID, uiMode string) (*session, error) {
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("line_items[0][price_data][currency]", "eur")
	form.Set("line_items[0][price_data][product_data][name]", "DPM flow demo T-shirt")
	form.Set("line_items[0][price_data][unit_amount]", "2000")
	form.Set("line_items[0][quantity]", "1")
	if pmcID != "" {
		form.Set("payment_method_configuration", pmcID)
	}
	if uiMode == "embedded_page" {
		form.Set("ui_mode", "embedded_page")
		form.Set("return_url", "https://example.com/return")
	} else {
		form.Set("success_url", "https://example.com/success")
	}
	version := ""
	if uiMode == "embedded_page" {
		version = "2026-03-25.dahlia"
	}
	var s session
	if err := stripePOSTv(key, "/v1/checkout/sessions", version, form, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// runFlow executes steps A and B headlessly, then leaves an embedded-page
// server running for step C if -flow-serve is set.
func runFlow(profile string, serveEmbedded bool) {
	key, err := loadTestKey(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "flow: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("== A. Session-diff loop (ephemeral config; your Default config is untouched)")

	// A1: ephemeral configuration.
	var pmc struct {
		ID string `json:"id"`
	}
	name := fmt.Sprintf("dpm-flow-demo-%d", os.Getpid())
	// A fresh configuration starts EMPTY (it does not inherit the account
	// defaults — verified empirically), so enable an EUR-friendly set here.
	createForm := url.Values{"name": {name}}
	for _, m := range []string{"card", "ideal", "bancontact", "eps", "sepa_debit"} {
		createForm.Set(m+"[display_preference][preference]", "on")
	}
	if err := stripePOST(key, "/v1/payment_method_configurations", createForm, &pmc); err != nil {
		fmt.Fprintf(os.Stderr, "flow: create config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  created ephemeral payment-method configuration %s (%q)\n", pmc.ID, name)

	// A2: session 1 against it.
	s1, err := createSession(key, pmc.ID, "hosted")
	if err != nil {
		fmt.Fprintf(os.Stderr, "flow: session 1: %v\n", err)
		os.Exit(1)
	}
	sort.Strings(s1.PaymentMethodTypes)
	fmt.Printf("  session 1 %s resolves %d methods: %s\n", s1.ID, len(s1.PaymentMethodTypes), strings.Join(s1.PaymentMethodTypes, ", "))

	// A3: toggle one non-card method off on the ephemeral config.
	toggle := ""
	for _, m := range s1.PaymentMethodTypes {
		if m != "card" {
			toggle = m
			break
		}
	}
	if toggle == "" {
		fmt.Println("  only card present; nothing to toggle — skipping diff")
	} else {
		form := url.Values{}
		form.Set(toggle+"[display_preference][preference]", "off")
		var upd map[string]any
		if err := stripePOST(key, "/v1/payment_method_configurations/"+pmc.ID, form, &upd); err != nil {
			fmt.Fprintf(os.Stderr, "flow: toggle %s off: %v\n", toggle, err)
		} else {
			fmt.Printf("  toggled %q OFF on the ephemeral config\n", toggle)
		}

		// A4: session 2, diff.
		s2, err := createSession(key, pmc.ID, "hosted")
		if err != nil {
			fmt.Fprintf(os.Stderr, "flow: session 2: %v\n", err)
			os.Exit(1)
		}
		sort.Strings(s2.PaymentMethodTypes)
		fmt.Printf("  session 2 %s resolves %d methods: %s\n", s2.ID, len(s2.PaymentMethodTypes), strings.Join(s2.PaymentMethodTypes, ", "))

		removed, added := diffSets(s1.PaymentMethodTypes, s2.PaymentMethodTypes)
		fmt.Printf("  DIFF: removed=%v added=%v\n", removed, added)
		if len(removed) == 1 && removed[0] == toggle && len(added) == 0 {
			fmt.Println("  VERIFIED: the dashboard config change alone changed what customers see — no code change involved")
		} else {
			fmt.Println("  UNEXPECTED DIFF — inspect manually")
		}
		fmt.Printf("  hosted URLs for browser verification:\n    before: %s\n    after:  %s\n", s1.URL, s2.URL)
	}

	// B: delayed-notification drill.
	fmt.Println("\n== B. Delayed-notification webhook drill (doc's recommended step)")
	drillOK := webhookDrill()

	if !drillOK {
		fmt.Println("  NOTE: webhook drill did not fully verify — see above")
	}

	if serveEmbedded {
		// C: embedded page server needs the config ACTIVE; deactivate later
		// with -flow-cleanup <config-id>.
		fmt.Printf("\n  config %s stays active while serving — run -flow-cleanup %s when done\n", pmc.ID, pmc.ID)
		serveEmbeddedPage(key, pmc.ID)
		return
	}
	deactivate(key, pmc.ID)
}

func deactivate(key, id string) {
	var deact map[string]any
	if err := stripePOST(key, "/v1/payment_method_configurations/"+id, url.Values{"active": {"false"}}, &deact); err != nil {
		fmt.Fprintf(os.Stderr, "  cleanup: deactivate %s: %v (deactivate manually in the dashboard)\n", id, err)
	} else {
		fmt.Printf("  cleanup: ephemeral config %s deactivated\n", id)
	}
}

func diffSets(a, b []string) (removed, added []string) {
	as, bs := map[string]bool{}, map[string]bool{}
	for _, x := range a {
		as[x] = true
	}
	for _, x := range b {
		bs[x] = true
	}
	for _, x := range a {
		if !bs[x] {
			removed = append(removed, x)
		}
	}
	for _, x := range b {
		if !as[x] {
			added = append(added, x)
		}
	}
	return
}

// webhookDrill implements the doc's webhook handler and proves it works by
// round-tripping a triggered event through `stripe listen`.
func webhookDrill() bool {
	received := make(chan string, 8)
	var secret string

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		sig := r.Header.Get("Stripe-Signature")
		if !verifySignature(payload, sig, secret) {
			w.WriteHeader(400)
			received <- "SIGNATURE-INVALID"
			return
		}
		var evt struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(payload, &evt)
		// The doc's three cases.
		switch evt.Type {
		case "checkout.session.completed",
			"checkout.session.async_payment_succeeded",
			"checkout.session.async_payment_failed":
			received <- evt.Type
		}
		w.WriteHeader(200)
	})
	srv := &http.Server{Addr: "127.0.0.1:4242", Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	defer srv.Close()
	fmt.Println("  local handler listening on 127.0.0.1:4242/webhook (completed / async_payment_succeeded / async_payment_failed)")

	stripeBin := os.Getenv("STRIPE_BIN")
	if stripeBin == "" {
		stripeBin = "stripe"
	}
	listen := exec.Command(stripeBin, "listen", "--forward-to", "127.0.0.1:4242/webhook",
		"--events", "checkout.session.completed,checkout.session.async_payment_succeeded,checkout.session.async_payment_failed")
	stderr, _ := listen.StderrPipe()
	if err := listen.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "  stripe listen failed to start: %v\n", err)
		return false
	}
	defer func() { _ = listen.Process.Kill(); _, _ = listen.Process.Wait() }()

	// The signing secret is printed on stderr as "... whsec_xxx ...".
	secretCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		acc := ""
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				acc += string(buf[:n])
				if i := strings.Index(acc, "whsec_"); i >= 0 {
					end := i
					for end < len(acc) && !strings.ContainsRune(" \n\r\t", rune(acc[end])) {
						end++
					}
					secretCh <- acc[i:end]
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case secret = <-secretCh:
		fmt.Println("  stripe listen ready (signing secret captured, not shown)")
	case <-time.After(20 * time.Second):
		fmt.Fprintln(os.Stderr, "  timed out waiting for stripe listen to become ready")
		return false
	}

	trigger := exec.Command(stripeBin, "trigger", "checkout.session.async_payment_succeeded")
	out, err := trigger.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  stripe trigger failed: %v\n%s\n", err, out)
		return false
	}
	fmt.Println("  triggered checkout.session.async_payment_succeeded")

	want := map[string]bool{"checkout.session.completed": false, "checkout.session.async_payment_succeeded": false}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case evt := <-received:
			if evt == "SIGNATURE-INVALID" {
				fmt.Println("  EVENT ARRIVED WITH BAD SIGNATURE — drill failed")
				return false
			}
			fmt.Printf("  received %-45s signature VERIFIED\n", evt)
			want[evt] = true
			all := true
			for _, got := range want {
				if !got {
					all = false
				}
			}
			if all {
				fmt.Println("  VERIFIED: the doc's delayed-notification handler round-trips end to end")
				return true
			}
		case <-deadline:
			got := []string{}
			for e, ok := range want {
				if ok {
					got = append(got, e)
				}
			}
			fmt.Printf("  timed out; events received: %v\n", got)
			return len(got) > 0
		}
	}
}

// verifySignature implements Stripe's v1 scheme: HMAC-SHA256 of "t.payload".
func verifySignature(payload []byte, header, secret string) bool {
	if secret == "" || header == "" {
		return false
	}
	var t string
	var v1s []string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			t = kv[1]
		case "v1":
			v1s = append(v1s, kv[1])
		}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	for _, v1 := range v1s {
		if hmac.Equal([]byte(expected), []byte(v1)) {
			return true
		}
	}
	return false
}

// serveEmbeddedPage hosts the doc's embedded variant locally: each page load
// creates a fresh embedded_page session and mounts it with Stripe.js.
func serveEmbeddedPage(key, pmcID string) {
	pub := loadPublishableKey()
	if pub == "" {
		fmt.Fprintln(os.Stderr, "  no test_mode_pub_key in CLI config; skipping embedded page")
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s, err := createSession(key, pmcID, "embedded_page")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, embeddedHTML, pub, s.ClientSecret)
	})
	fmt.Println("\n== C. Embedded variant: http://127.0.0.1:4243 (each load creates a fresh embedded_page session)")
	_ = http.ListenAndServe("127.0.0.1:4243", mux)
}

func loadPublishableKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	var cfg map[string]map[string]any
	if _, err := toml.DecodeFile(home+"/.config/stripe/config.toml", &cfg); err != nil {
		return ""
	}
	if p, ok := cfg["default"]; ok {
		if k, ok := p["test_mode_pub_key"].(string); ok {
			return k
		}
	}
	return ""
}

const embeddedHTML = `<!DOCTYPE html>
<html>
<head><title>DPM flow — embedded checkout demo</title>
<script src="https://js.stripe.com/v3/"></script></head>
<body>
<h3>Embedded checkout (ui_mode=embedded_page) — methods come from the ephemeral config</h3>
<div id="checkout"></div>
<script>
  const stripe = Stripe(%q);
  stripe.initEmbeddedCheckout({clientSecret: %q}).then(c => c.mount('#checkout'));
</script>
</body>
</html>`
