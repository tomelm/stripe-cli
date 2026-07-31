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
	"net"
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

// experimentRun proves the doc's central claim live: an EPHEMERAL
// payment-method configuration (the account's Default config is never
// touched) drives which methods a Checkout Session resolves. keepActive
// leaves the config active (for the embedded server); otherwise it is
// deactivated before returning.
func experimentRun(profile string, keepActive bool) (*ExperimentReport, error) {
	key, err := loadTestKey(profile)
	if err != nil {
		return nil, err
	}
	r := &ExperimentReport{Command: "experiment", Removed: []string{}, Added: []string{}}

	var pmc struct {
		ID string `json:"id"`
	}
	name := fmt.Sprintf("dpm-flow-demo-%d", os.Getpid())
	// A fresh configuration starts EMPTY (it does not inherit account
	// defaults — verified empirically), so enable an EUR-friendly set.
	createForm := url.Values{"name": {name}}
	for _, m := range []string{"card", "ideal", "bancontact", "eps", "sepa_debit"} {
		createForm.Set(m+"[display_preference][preference]", "on")
	}
	if err := stripePOST(key, "/v1/payment_method_configurations", createForm, &pmc); err != nil {
		return nil, fmt.Errorf("create ephemeral configuration: %w", err)
	}
	r.ConfigID = pmc.ID

	cleanup := func() {
		if keepActive {
			r.Cleanup = "kept-active"
			return
		}
		if derr := deactivate(key, pmc.ID); derr != nil {
			r.Cleanup = "FAILED — run `dpm cleanup " + pmc.ID + "`"
			return
		}
		r.Cleanup = "deactivated"
	}

	s1, err := createSession(key, pmc.ID, "hosted")
	if err != nil {
		cleanup()
		return r, fmt.Errorf("session 1: %w", err)
	}
	sort.Strings(s1.PaymentMethodTypes)
	r.Before = SessionInfo{ID: s1.ID, URL: s1.URL, Methods: s1.PaymentMethodTypes}

	for _, m := range s1.PaymentMethodTypes {
		if m != "card" {
			r.Toggled = m
			break
		}
	}
	if r.Toggled == "" {
		cleanup()
		return r, fmt.Errorf("only card resolved; nothing to toggle")
	}
	form := url.Values{}
	form.Set(r.Toggled+"[display_preference][preference]", "off")
	var upd map[string]any
	if err := stripePOST(key, "/v1/payment_method_configurations/"+pmc.ID, form, &upd); err != nil {
		cleanup()
		return r, fmt.Errorf("toggle %s off: %w", r.Toggled, err)
	}

	s2, err := createSession(key, pmc.ID, "hosted")
	if err != nil {
		cleanup()
		return r, fmt.Errorf("session 2: %w", err)
	}
	sort.Strings(s2.PaymentMethodTypes)
	r.After = SessionInfo{ID: s2.ID, URL: s2.URL, Methods: s2.PaymentMethodTypes}

	r.Removed, r.Added = diffSets(s1.PaymentMethodTypes, s2.PaymentMethodTypes)
	r.Verified = len(r.Removed) == 1 && r.Removed[0] == r.Toggled && len(r.Added) == 0
	cleanup()
	return r, nil
}

// deactivate is silent; callers report the outcome (the experiment report's
// cleanup field, or the cleanup command's own message).
func deactivate(key, id string) error {
	var deact map[string]any
	return stripePOST(key, "/v1/payment_method_configurations/"+id, url.Values{"active": {"false"}}, &deact)
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

// drillRun implements the doc's delayed-notification handler and proves it
// works by round-tripping a triggered event through `stripe listen`. The
// local handler binds an OS-assigned port, so nothing collides with the
// classic 4242.
func drillRun() (*DrillReport, error) {
	r := &DrillReport{Command: "drill", Events: []DrillEvent{}}
	received := make(chan DrillEvent, 8)
	var secret string

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return r, err
	}
	addr := ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, req *http.Request) {
		payload, _ := io.ReadAll(req.Body)
		sig := req.Header.Get("Stripe-Signature")
		var evt struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(payload, &evt)
		if !verifySignature(payload, sig, secret) {
			w.WriteHeader(400)
			received <- DrillEvent{Type: evt.Type, Signature: "invalid"}
			return
		}
		switch evt.Type {
		case "checkout.session.completed",
			"checkout.session.async_payment_succeeded",
			"checkout.session.async_payment_failed":
			received <- DrillEvent{Type: evt.Type, Signature: "verified"}
		}
		w.WriteHeader(200)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	stripeBin := os.Getenv("STRIPE_BIN")
	if stripeBin == "" {
		if p, err := exec.LookPath("stripe"); err == nil {
			stripeBin = p
		} else {
			return r, fmt.Errorf("stripe CLI not found on PATH (set STRIPE_BIN)")
		}
	}
	listen := exec.Command(stripeBin, "listen", "--forward-to", addr+"/webhook",
		"--events", "checkout.session.completed,checkout.session.async_payment_succeeded,checkout.session.async_payment_failed")
	stderrPipe, _ := listen.StderrPipe()
	if err := listen.Start(); err != nil {
		return r, fmt.Errorf("stripe listen: %w", err)
	}
	defer func() { _ = listen.Process.Kill(); _, _ = listen.Process.Wait() }()

	secretCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		acc := ""
		for {
			n, err := stderrPipe.Read(buf)
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
		r.ListenReady = true
	case <-time.After(20 * time.Second):
		return r, fmt.Errorf("timed out waiting for stripe listen")
	}

	trigger := exec.Command(stripeBin, "trigger", "checkout.session.async_payment_succeeded")
	if out, err := trigger.CombinedOutput(); err != nil {
		return r, fmt.Errorf("stripe trigger: %v: %s", err, out)
	}
	r.Triggered = "checkout.session.async_payment_succeeded"

	want := map[string]bool{"checkout.session.completed": false, "checkout.session.async_payment_succeeded": false}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case evt := <-received:
			r.Events = append(r.Events, evt)
			if evt.Signature != "verified" {
				return r, fmt.Errorf("event %s arrived with invalid signature", evt.Type)
			}
			want[evt.Type] = true
			all := true
			for _, got := range want {
				if !got {
					all = false
				}
			}
			if all {
				r.Verified = true
				return r, nil
			}
		case <-deadline:
			r.Note = "timed out before both events arrived"
			return r, nil
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
