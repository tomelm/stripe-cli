package coopcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
	"github.com/stripe/stripe-cli/pkg/parsers"
	"github.com/stripe/stripe-cli/pkg/requests"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

// debugLiveExecutor lets the debug agent really execute a blueprint's
// apiRequest nodes against test-mode Stripe, so a human can drive a genuine
// journey (pay a real Checkout Session with the test card) while the TUI's
// outcome observer watches the real object. Blueprint ${node.<step>.<key>:
// <field>} references resolve against earlier live responses, stored under
// the same "node.<step>.<key>" names the blueprint uses.
type debugLiveExecutor struct {
	apiKey    string
	responses map[string]gjson.Result

	// cartAppURL caches the local storefront serveCartApp starts for
	// checkout_session journeys, so repeated bindingFor calls for the same
	// review reuse one listener instead of spawning a new one each time.
	cartAppURL string
}

func newDebugLiveExecutor() (*debugLiveExecutor, error) {
	if options.TestModeAPIKey == nil {
		return nil, fmt.Errorf("--live requires a configured test-mode API key")
	}
	apiKey, err := options.TestModeAPIKey()
	if err != nil || strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("--live requires a test-mode API key: run stripe login, or set STRIPE_API_KEY")
	}
	if strings.HasPrefix(apiKey, "sk_live_") || strings.HasPrefix(apiKey, "rk_live_") {
		return nil, fmt.Errorf("--live refuses live-mode keys; use a test-mode key")
	}
	return &debugLiveExecutor{apiKey: apiKey, responses: map[string]gjson.Result{}}, nil
}

var liveNodeRefPattern = regexp.MustCompile(`\$\{(node\.[^:}]+):([^}|]+)\}`)

var liveEnvRefPattern = regexp.MustCompile(`\$\{env:([^}]+)\}`)

// liveEnvDefaults are debug-live pragmatic defaults for Workbench-style
// ${env:...} tokens some upstream blueprints carry; session settings win.
var liveEnvDefaults = map[string]string{
	"currency": "usd",
}

// resolveEnvTokens substitutes ${env:<name>} tokens (Workbench blueprint
// syntax the fixture parsers do not know) from session settings, falling back
// to debug defaults. Errors on tokens with no resolution rather than sending
// the literal to the API.
func resolveEnvTokens(params interface{}, settings map[string]string) (interface{}, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var resolveErr error
	replaced := liveEnvRefPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		name := string(liveEnvRefPattern.FindSubmatch(match)[1])
		if value, ok := settings[name]; ok && value != "" {
			return []byte(value)
		}
		if value, ok := liveEnvDefaults[name]; ok {
			return []byte(value)
		}
		resolveErr = fmt.Errorf("no value for ${env:%s} (set it with --setting %s=<value>)", name, name)
		return match
	})
	if resolveErr != nil {
		return nil, resolveErr
	}
	var resolved interface{}
	if err := json.Unmarshal(replaced, &resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

// execute performs the node's API request for real and records the response
// for later reference resolution. Returns the parsed response body.
func (l *debugLiveExecutor) execute(ctx context.Context, session *coop.Session, nodeNumber int, node *coop.SessionNode) (gjson.Result, error) {
	step, _, _, err := session.StepByNodeNumber(nodeNumber)
	if err != nil {
		return gjson.Result{}, err
	}
	path := l.interpolate(node.Request.Path)
	resolvedParams, err := resolveEnvTokens(node.Request.Params, session.Settings)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("resolving env tokens for %s: %w", path, err)
	}
	formData, err := parsers.ParseToFormData(resolvedParams, l.responses)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("resolving params for %s: %w", path, err)
	}
	params := &requests.RequestParameters{}
	params.AppendData(formData)

	base := requests.Base{
		Method:         strings.ToUpper(node.Request.Method),
		SuppressOutput: true,
		APIBaseURL:     stripe.DefaultAPIBaseURL,
	}
	configure := func(req *http.Request) error {
		for name, value := range node.Request.Headers {
			req.Header.Set(name, l.interpolate(value))
		}
		return nil
	}
	body, err := base.MakeRequest(ctx, l.apiKey, path, params, map[string]interface{}{}, true, configure)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("%s %s: %w", base.Method, path, err)
	}
	result := gjson.ParseBytes(body)
	l.responses["node."+step.Key+"."+node.Key] = result
	return result, nil
}

// interpolate resolves ${node.<step>.<key>:<field>} references in a string
// against recorded live responses (used for paths and header values; params
// go through parsers.ParseToFormData which handles the same syntax).
func (l *debugLiveExecutor) interpolate(value string) string {
	return liveNodeRefPattern.ReplaceAllStringFunc(value, func(match string) string {
		groups := liveNodeRefPattern.FindStringSubmatch(match)
		if len(groups) != 3 {
			return match
		}
		response, ok := l.responses[groups[1]]
		if !ok {
			return match
		}
		if resolved := response.Get(groups[2]).String(); resolved != "" {
			return resolved
		}
		return match
	})
}

// bindingFor finds the most recently created live object matching the
// expectation's id prefix, plus the journey URL its object exposes. For
// payment intents (no hosted URL) it serves a minimal local page that mounts
// a real Payment Element, so the human can genuinely complete an embedded
// journey in a browser.
//
// checkout_session is different in kind, not just presentation: app-entry
// verification (pkg/coop/workflow/ui_gate.go's validateAppEntryURL) refuses a
// checkout.stripe.com URL as the journey's starting point, on purpose — a
// blueprint's apiRequest node minting a Session directly proves the API call
// works, not that the developer's own app can drive a customer to one. So for
// that role we never hand back a pre-created session's hosted url; instead we
// return no id at all (the checker's discover pass finds whatever object the
// app mints) and point the journey at a tiny local storefront that creates
// the real Session itself when the human clicks Buy.
func (l *debugLiveExecutor) bindingFor(expectation uicheck.Expectation) (id, journeyURL string) {
	if expectation.Role == "checkout_session" {
		if priceID := l.priceIDFromResponses(); priceID != "" {
			return "", l.serveCartApp(priceID)
		}
		// No price known yet (the blueprint's product-creation node hasn't
		// run, or this blueprint doesn't have one) - fall back to the
		// pre-change behavior below rather than binding nothing at all.
	}

	for _, response := range l.responses {
		objectID := response.Get("id").String()
		if !strings.HasPrefix(objectID, expectation.IDPrefix) {
			continue
		}
		id = objectID
		for _, field := range []string{"url", "hosted_invoice_url"} {
			if value := response.Get(field).String(); strings.HasPrefix(value, "https://") {
				journeyURL = value
				break
			}
		}
		if journeyURL == "" && expectation.Role == "payment_intent" {
			if clientSecret := response.Get("client_secret").String(); clientSecret != "" {
				journeyURL = l.serveEmbeddedPaymentPage(clientSecret)
			}
		}
		return id, journeyURL
	}
	return "", ""
}

// priceIDFromResponses finds a price id among the live responses recorded so
// far, so serveCartApp has something to sell. Blueprints either create a
// standalone Price (id itself is the match) or a Product with inline
// default_price_data, whose response carries the generated price id under
// default_price - the one-time-payment blueprint's create-product node is the
// latter shape.
func (l *debugLiveExecutor) priceIDFromResponses() string {
	for _, response := range l.responses {
		if id := response.Get("id").String(); strings.HasPrefix(id, "price_") {
			return id
		}
		if defaultPrice := response.Get("default_price").String(); strings.HasPrefix(defaultPrice, "price_") {
			return defaultPrice
		}
	}
	return ""
}

// embeddedPaymentPage is the minimal real Payment Element surface for live
// debug sessions: Stripe.js mounts an actual Element against the created
// PaymentIntent, so the browser journey (and its Stripe-side consequence)
// is genuine. Test-mode publishable key + client secret only.
const embeddedPaymentPage = `<!doctype html>
<html><head><title>coop debug: Payment Element</title>
<script src="https://js.stripe.com/v3/"></script></head>
<body style="font-family: sans-serif; max-width: 28rem; margin: 4rem auto;">
<h3>coop debug payment</h3>
<form id="f"><div id="payment-element"></div>
<button style="margin-top:1rem" id="submit">Pay</button>
<div id="msg" role="alert"></div></form>
<script>
const stripe = Stripe(%q);
const clientSecret = %q;
const elements = stripe.elements({clientSecret});
elements.create("payment").mount("#payment-element");
document.getElementById("f").addEventListener("submit", async (e) => {
  e.preventDefault();
  const {error} = await stripe.confirmPayment({elements, redirect: "if_required"});
  document.getElementById("msg").textContent = error ? error.message : "Payment submitted - you can close this tab.";
});
</script></body></html>`

// serveEmbeddedPaymentPage starts a localhost server for the Payment Element
// page and returns its URL. The server lives for the debug agent's lifetime.
func (l *debugLiveExecutor) serveEmbeddedPaymentPage(clientSecret string) string {
	publishableKey := ""
	if options.TestModePublishableKey != nil {
		if key, err := options.TestModePublishableKey(); err == nil {
			publishableKey = strings.TrimSpace(key)
		}
	}
	if publishableKey == "" || !strings.HasPrefix(publishableKey, "pk_test_") {
		return ""
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ""
	}
	page := fmt.Sprintf(embeddedPaymentPage, publishableKey, clientSecret)
	go func() {
		_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, page)
		}))
	}()
	return "http://" + listener.Addr().String() + "/"
}

// cartAppPage is a stand-in for a developer's own storefront: a product, a
// price, and a plain form (no Stripe.js - the Session doesn't exist until the
// server route below mints it). The point is not the HTML, it's that the
// journey's entry point is this app's own page, not a Stripe one.
const cartAppPage = `<!doctype html>
<html><head><title>coop debug: cart</title>
<style>body{font-family:sans-serif;max-width:28rem;margin:4rem auto}
button{font-size:1rem;padding:0.5rem 1.25rem;cursor:pointer}</style></head>
<body>
<h3>coop debug cart</h3>
<p>%s &mdash; %s</p>
<form method="post" action="/checkout">
<button type="submit">Buy now</button>
</form>
</body></html>`

// cartSuccessPage is returned at success_url once Checkout redirects back.
const cartSuccessPage = `<!doctype html>
<html><head><title>coop debug: success</title></head>
<body style="font-family: sans-serif; max-width: 28rem; margin: 4rem auto;">
<h3>Thanks &mdash; payment complete</h3>
<p>You can close this tab.</p>
</body></html>`

// serveCartApp starts a localhost storefront for priceID and returns its
// base URL. Unlike serveEmbeddedPaymentPage, the object under test isn't
// created before the browser opens: the /checkout route mints a fresh
// Checkout Session, server-side, the moment the human clicks Buy - exactly
// the app-minted shape the discover pass in pkg/coop/uicheck/checker.go is
// built to find. Cached on the struct so one review only ever gets one
// listener.
func (l *debugLiveExecutor) serveCartApp(priceID string) string {
	if l.cartAppURL != "" {
		return l.cartAppURL
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ""
	}
	base := "http://" + listener.Addr().String()
	name, price := l.fetchCartLabel(priceID)
	cartPage := fmt.Sprintf(cartAppPage, name, price)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, cartPage)
	})
	mux.HandleFunc("/success", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, cartSuccessPage)
	})
	mux.HandleFunc("/checkout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		params := &requests.RequestParameters{}
		params.AppendData([]string{
			"mode=payment",
			"line_items[0][price]=" + priceID,
			"line_items[0][quantity]=1",
			"success_url=" + base + "/success",
			"cancel_url=" + base + "/",
		})
		checkoutBase := requests.Base{Method: "POST", SuppressOutput: true, APIBaseURL: stripe.DefaultAPIBaseURL}
		body, err := checkoutBase.MakeRequest(r.Context(), l.apiKey, "/v1/checkout/sessions", params, map[string]interface{}{}, true, nil)
		if err != nil {
			// The message alone (never l.apiKey) is enough for a human to see
			// what went wrong from the browser.
			http.Error(w, "creating checkout session: "+err.Error(), http.StatusInternalServerError)
			return
		}
		sessionURL := gjson.GetBytes(body, "url").String()
		if sessionURL == "" {
			http.Error(w, "checkout session response had no hosted url", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, sessionURL, http.StatusSeeOther)
	})

	go func() {
		_ = http.Serve(listener, mux)
	}()
	l.cartAppURL = base + "/"
	return l.cartAppURL
}

// fetchCartLabel best-effort fetches a price's amount and product name (via
// expand[]=product) to make the cart page read like a real product instead
// of a bare id. Failure just falls back to generic copy - this is cosmetic,
// never load-bearing for the journey itself.
func (l *debugLiveExecutor) fetchCartLabel(priceID string) (name, price string) {
	name, price = "your product", "the configured price"
	params := &requests.RequestParameters{}
	params.AppendExpand([]string{"product"})
	base := requests.Base{Method: "GET", SuppressOutput: true, APIBaseURL: stripe.DefaultAPIBaseURL}
	body, err := base.MakeRequest(context.Background(), l.apiKey, "/v1/prices/"+priceID, params, map[string]interface{}{}, true, nil)
	if err != nil {
		return name, price
	}
	result := gjson.ParseBytes(body)
	if productName := result.Get("product.name").String(); productName != "" {
		name = productName
	}
	if amount := result.Get("unit_amount"); amount.Exists() && amount.Int() > 0 {
		currency := strings.ToUpper(result.Get("currency").String())
		price = fmt.Sprintf("%.2f %s", float64(amount.Int())/100, currency)
	}
	return name, price
}
