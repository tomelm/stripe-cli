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
func (l *debugLiveExecutor) bindingFor(expectation uicheck.Expectation) (id, journeyURL string) {
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
