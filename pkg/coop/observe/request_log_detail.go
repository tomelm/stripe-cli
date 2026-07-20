package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/stripe/stripe-cli/pkg/stripe"
)

// RequestSourceClass is a bounded classification of who made an observed
// request, derived from the request log's metadata. The raw User-Agent is
// never retained.
type RequestSourceClass string

const (
	RequestSourceAgent     RequestSourceClass = "agent"
	RequestSourceCLI       RequestSourceClass = "cli"
	RequestSourceDashboard RequestSourceClass = "dashboard"
	RequestSourceSDK       RequestSourceClass = "sdk"
	RequestSourceUnknown   RequestSourceClass = "unknown"
)

// RequestLogDetail is the bounded projection of one request log's detail.
// Param VALUES, bodies, headers, and key material are never retained — only
// top-level param key names and the source classification.
type RequestLogDetail struct {
	ParamKeys     []string
	FromDashboard bool
	SourceClass   RequestSourceClass
	Livemode      bool
}

// RequestLogFetcher loads bounded detail for an observed request log.
type RequestLogFetcher interface {
	Fetch(ctx context.Context, requestID string) (RequestLogDetail, error)
}

// stripeRequestLogFetcher fetches request-log detail over the CLI's standard
// Stripe client routing. The endpoint is undocumented; any failure is treated
// as detail-unavailable by callers, never as a verification signal.
type stripeRequestLogFetcher struct {
	apiKey  string
	baseURL *url.URL
}

// NewRequestLogFetcher returns the production fetcher for the given key.
func NewRequestLogFetcher(apiKey string) RequestLogFetcher {
	base, _ := url.Parse(stripe.DefaultAPIBaseURL)
	return &stripeRequestLogFetcher{apiKey: apiKey, baseURL: base}
}

// requestLogDetailPayload mirrors only the fields we project; everything else
// in the response is ignored and discarded.
type requestLogDetailPayload struct {
	Livemode bool `json:"livemode"`
	Request  struct {
		FromDashboard bool                       `json:"from_dashboard"`
		GetParams     map[string]json.RawMessage `json:"get_params"`
		PostParams    map[string]json.RawMessage `json:"post_params"`
		Headers       struct {
			UserAgent string `json:"User-Agent"`
		} `json:"headers"`
	} `json:"request"`
}

func (fetcher *stripeRequestLogFetcher) Fetch(ctx context.Context, requestID string) (RequestLogDetail, error) {
	if err := validateConfigText("request_id", requestID, 128, false); err != nil {
		return RequestLogDetail{}, err
	}
	if fetcher.baseURL == nil {
		return RequestLogDetail{}, fmt.Errorf("request log fetcher has no API base")
	}
	// BaseURL must be set: stripe.Client.PerformRequest dereferences it.
	client := &stripe.Client{APIKey: fetcher.apiKey, BaseURL: fetcher.baseURL}
	telemetrySafeCtx := stripe.WithTelemetryClient(ctx, &stripe.NoOpTelemetryClient{})
	resp, err := client.PerformRequest(telemetrySafeCtx, http.MethodGet, "/v1/request_logs/"+requestID, "", nil)
	if err != nil {
		return RequestLogDetail{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
		return RequestLogDetail{}, fmt.Errorf("request log detail unavailable (HTTP %d)", resp.StatusCode)
	}
	var payload requestLogDetailPayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, passiveWebSocketReadLimit)).Decode(&payload); err != nil {
		return RequestLogDetail{}, err
	}
	return boundRequestLogDetail(payload), nil
}

func boundRequestLogDetail(payload requestLogDetailPayload) RequestLogDetail {
	keys := make(map[string]bool, len(payload.Request.PostParams)+len(payload.Request.GetParams))
	for key := range payload.Request.PostParams {
		keys[key] = true
	}
	for key := range payload.Request.GetParams {
		keys[key] = true
	}
	detail := RequestLogDetail{
		ParamKeys:     sortedKeys(keys),
		FromDashboard: payload.Request.FromDashboard,
		Livemode:      payload.Livemode,
	}
	detail.SourceClass = classifyRequestSource(payload.Request.FromDashboard, payload.Request.Headers.UserAgent)
	return detail
}

// classifyRequestSource maps request metadata to a bounded source token.
func classifyRequestSource(fromDashboard bool, userAgent string) RequestSourceClass {
	switch {
	case fromDashboard:
		return RequestSourceDashboard
	case strings.Contains(userAgent, "AIAgent/"):
		return RequestSourceAgent
	case strings.Contains(userAgent, "stripe-cli"):
		return RequestSourceCLI
	case strings.HasPrefix(userAgent, "Stripe/v1 ") || strings.HasPrefix(userAgent, "Stripe/v2 "):
		return RequestSourceSDK
	case userAgent == "":
		return RequestSourceUnknown
	default:
		return RequestSourceUnknown
	}
}

// paramPresence compares blueprint-declared param keys against the observed
// request's keys, returning how many were present and up to limit missing key
// names (identifiers only, never values).
func paramPresence(expected []string, observed []string, limit int) (present int, missing []string) {
	observedSet := make(map[string]bool, len(observed))
	for _, key := range observed {
		observedSet[key] = true
	}
	sortedExpected := append([]string(nil), expected...)
	sort.Strings(sortedExpected)
	for _, key := range sortedExpected {
		if observedSet[key] {
			present++
		} else if len(missing) < limit {
			missing = append(missing, key)
		}
	}
	return present, missing
}
