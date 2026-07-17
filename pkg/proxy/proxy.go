package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/stripe/stripe-cli/pkg/ansi"
	"github.com/stripe/stripe-cli/pkg/config"
	"github.com/stripe/stripe-cli/pkg/requests"
	"github.com/stripe/stripe-cli/pkg/stripe"
	"github.com/stripe/stripe-cli/pkg/stripeauth"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

//
// Public types
//

// EndpointRoute describes a local endpoint's routing configuration.
type EndpointRoute struct {
	// URL is the endpoint's URL.
	URL string

	// Headers to forward to endpoints
	ForwardHeaders []string

	// Connect indicates whether the endpoint should receive normal (when false) or Connect (when true) events.
	Connect bool

	// EventTypes is the list of event types that should be sent to the endpoint.
	EventTypes []string

	// Status is whether or not the endpoint is enabled.
	Status string

	// IsEventDestination indicates whether this is a Thin endpoint
	IsEventDestination bool
}

// EndpointResponse describes the response to a Stripe event from an endpoint
type EndpointResponse struct {
	Event   *StripeEvent
	V2Event *V2EventPayload
	Resp    *http.Response
}

// FailedToReadResponseError describes a failure to read the response from an endpoint
type FailedToReadResponseError struct {
	Err error
}

func (f FailedToReadResponseError) Error() string {
	return f.Err.Error()
}

// Config provides the configuration of a Proxy
type Config struct {
	// DeviceName is the name of the device sent to Stripe to help identify the device
	DeviceName  string
	DeviceToken *string

	// Client is a configured stripe client used to execute authenticated calls to the Stripe API.
	Client stripe.RequestPerformer

	// URL to which events are forwarded to
	ForwardURL string
	// URL to which Thin events are forwarded to
	ForwardThinURL string
	// Headers to inject when forwarding events
	ForwardHeaders []string
	// URL to which Connect events are forwarded to
	ForwardConnectURL string
	// URL to which Connect Thin events are forwarded to
	ForwardThinConnectURL string
	// Headers to inject when forwarding Connect events
	ForwardConnectHeaders []string
	// UseConfiguredWebhooks loads webhooks config from user's account
	UseConfiguredWebhooks bool

	// List of events to listen and proxy
	Events []string
	// List of Thin-type events to listen and proxy
	ThinEvents []string

	// WebSocketFeatures is the feature specified for the websocket connection
	WebSocketFeatures []string
	// Indicates whether to print full JSON objects to stdout
	PrintJSON bool

	// Specifies the format to print to stdout.
	Format string

	// Indicates whether to filter events formatted with the default or latest API version
	UseLatestAPIVersion bool
	// Indicates whether to skip certificate verification when forwarding webhooks to HTTPS endpoints
	SkipVerify bool
	// The logger used to log messages to stdin/err
	Log *log.Logger
	// Force use of unencrypted ws:// protocol instead of wss://
	NoWSS bool
	// Override default timeout
	Timeout int64

	// OutCh is the channel to send logs and statuses to for processing in other packages
	OutCh chan websocket.IElement

	// WebSocketDialer, when non-nil, is used verbatim instead of the CLI's
	// ambient proxy/Unix-socket-aware dialer.
	WebSocketDialer websocket.Dialer

	// WebSocketReadLimit bounds one inbound message before JSON decoding. Zero
	// preserves the existing listen behavior.
	WebSocketReadLimit int64

	// WebSocketConnectAttemptWait overrides the delay between WebSocket connect
	// attempts. Zero preserves the existing default.
	WebSocketConnectAttemptWait time.Duration

	// ReportConnectionGaps emits Reconnecting before an established WebSocket
	// is reset or reconnects after a network failure.
	ReportConnectionGaps bool

	// SynchronousEventHandling keeps WebSocket payload production owned by Run
	// so output can be closed only after all producers have terminated.
	SynchronousEventHandling bool

	// OmitReadySecret prevents the session signing secret from entering OutCh.
	// Passive observers that never forward HTTP deliveries should enable it.
	OmitReadySecret bool

	// OmitMarshaledPayload prevents duplicate raw event JSON from entering OutCh
	// when the structured event is sufficient.
	OmitMarshaledPayload bool

	// DisconnectOnMalformedPayload reports malformed WebSocket JSON and malformed
	// event payloads as coverage-breaking reconnects.
	DisconnectOnMalformedPayload bool

	// LoggedInAccountID is the currently logged-in account ID
	LoggedInAccountID string
}

// A Proxy opens a websocket connection with Stripe, listens for incoming
// webhook events, forwards them to the local endpoint and sends the response
// back to Stripe.
type Proxy struct {
	cfg *Config

	stripeAuthClient      *stripeauth.Client
	webSocketClient       *websocket.Client
	webhookEventProcessor *WebhookEventProcessor
}

const maxConnectAttempts = 3

// IsConnected returns a channel that signals the proxy has finished connecting.
// can only be called after webSocketClient is initialized
func (p *Proxy) IsConnected() <-chan struct{} {
	for p.webSocketClient == nil {
		time.Sleep(50 * time.Millisecond)
	}
	return p.webSocketClient.Connected()
}

func (p *Proxy) sendMessage(msg *websocket.OutgoingMessage) {
	if p.webSocketClient != nil {
		p.webSocketClient.SendMessage(msg)
	}
}

// Run sets the websocket connection and starts the Goroutines to forward
// incoming events to the local endpoint.
func (p *Proxy) Run(ctx context.Context) error {
	defer close(p.cfg.OutCh)

	p.cfg.OutCh <- websocket.StateElement{
		State: websocket.Loading,
	}

	nAttempts := 0

	for nAttempts < maxConnectAttempts {
		session, err := p.createSession(ctx)
		if err != nil {
			p.cfg.OutCh <- websocket.ErrorElement{
				Error: fmt.Errorf("Error while authenticating with Stripe: %v", err),
			}
			return err
		}

		*p.cfg.DeviceToken = session.DeviceToken
		var readyOnce sync.Once
		connectedOnce := false
		p.webSocketClient = websocket.NewClient(
			session.WebSocketURL,
			session.WebSocketID,
			session.WebSocketAuthorizedFeature,
			&websocket.Config{
				Log:                          p.cfg.Log,
				Dialer:                       p.cfg.WebSocketDialer,
				ConnectAttemptWait:           p.cfg.WebSocketConnectAttemptWait,
				NoWSS:                        p.cfg.NoWSS,
				ReadLimit:                    p.cfg.WebSocketReadLimit,
				ReconnectInterval:            time.Duration(session.ReconnectDelay) * time.Second,
				EventHandler:                 p.webhookEventProcessor,
				SynchronousEventHandling:     p.cfg.SynchronousEventHandling,
				DisconnectOnMalformedMessage: p.cfg.DisconnectOnMalformedPayload,
				OnConnect: func() {
					connectedOnce = true
					readyOnce.Do(func() {
						displayedAPIVersion := ""
						if p.cfg.UseLatestAPIVersion && session.LatestVersion != "" {
							displayedAPIVersion = "You are using Stripe API Version [" + session.LatestVersion + "]. "
						} else if !p.cfg.UseLatestAPIVersion && session.DefaultVersion != "" {
							displayedAPIVersion = "You are using Stripe API Version [" + session.DefaultVersion + "]. "
						}
						readyData := []string{displayedAPIVersion, session.Secret}
						if p.cfg.OmitReadySecret {
							readyData = nil
						}
						select {
						case p.cfg.OutCh <- websocket.StateElement{
							State: websocket.Ready,
							Data:  readyData,
						}:
						case <-ctx.Done():
						}
					})
				},
				OnDisconnect: func() {
					if p.cfg.ReportConnectionGaps {
						select {
						case p.cfg.OutCh <- websocket.StateElement{State: websocket.Reconnecting}:
						case <-ctx.Done():
						}
					}
				},
			},
		)

		webSocketDone := make(chan struct{})
		nAttempts++
		go func() {
			p.webSocketClient.Run(ctx)
			close(webSocketDone)
		}()

		select {
		case <-ctx.Done():
			<-webSocketDone
			p.cfg.OutCh <- &websocket.StateElement{
				State: websocket.Done,
			}
			return nil
		case <-p.webSocketClient.NotifyExpired:
			<-webSocketDone
			if connectedOnce {
				nAttempts = 0
			}
			if nAttempts < maxConnectAttempts {
				p.cfg.OutCh <- &websocket.StateElement{
					State: websocket.Reconnecting,
				}
			} else {
				err := fmt.Errorf("session expired, terminating after %d failed attempts to reauthorize", nAttempts)
				p.cfg.OutCh <- websocket.ErrorElement{
					Error: err,
				}
				return err
			}
		}
	}

	if p.webSocketClient != nil {
		p.webSocketClient.Stop()
	}

	log.WithFields(log.Fields{
		"prefix": "proxy.Proxy.Run",
	}).Debug("Bye!")

	return nil
}

// GetSessionSecret creates a session and returns the webhook signing secret.
func GetSessionSecret(ctx context.Context, client stripe.RequestPerformer, deviceName string) (string, error) {
	p, err := Init(ctx, &Config{
		Client:            client,
		DeviceName:        deviceName,
		WebSocketFeatures: []string{"webhooks"},
	})
	if err != nil {
		log.WithFields(log.Fields{
			"prefix": "proxy.Proxy.GetSessionSecret",
		}).Debug(err)
		return "", err
	}

	session, err := p.createSession(ctx)
	if err != nil {
		log.WithFields(log.Fields{
			"prefix": "proxy.Proxy.GetSessionSecret",
		}).Debug(fmt.Sprintf("Error while authenticating with Stripe: %v", err))
		return "", err
	}

	return session.Secret, nil
}

func (p *Proxy) createSession(ctx context.Context) (*stripeauth.StripeCLISession, error) {
	var session *stripeauth.StripeCLISession

	var err error

	exitCh := make(chan struct{})

	go func() {
		// Try to authorize at least 5 times before failing. Sometimes we have random
		// transient errors that we just need to retry for.
		for i := 0; i <= 5; i++ {
			devURLMap := stripeauth.DeviceURLMap{
				ForwardURL:            p.cfg.ForwardURL,
				ForwardConnectURL:     p.cfg.ForwardConnectURL,
				ForwardThinURL:        p.cfg.ForwardThinURL,
				ForwardThinConnectURL: p.cfg.ForwardThinConnectURL,
			}

			session, err = p.stripeAuthClient.Authorize(ctx, stripeauth.CreateSessionRequest{
				DeviceName:        p.cfg.DeviceName,
				WebSocketFeatures: p.cfg.WebSocketFeatures,
				DeviceURLMap:      &devURLMap,
			})

			if err == nil {
				exitCh <- struct{}{}
				return
			}

			if clientError, ok := stripeauth.IsAuthorizationClientError(err); ok {
				if clientError.StatusCode == http.StatusTooManyRequests {
					err = errors.New("you have too many `stripe listen` sessions open, please close some and try again")
				}
				exitCh <- struct{}{}
				return
			}

			select {
			case <-ctx.Done():
				exitCh <- struct{}{}
				return
			case <-time.After(1 * time.Second):
			}
		}

		exitCh <- struct{}{}
	}()
	<-exitCh

	return session, err
}

// This function outputs the event payload in the format specified.
// Currently only supports JSON.
func formatOutput(format string, eventPayload string) string {
	var event map[string]interface{}
	err := json.Unmarshal([]byte(eventPayload), &event)
	if err != nil {
		return fmt.Sprintf("Received malformed event: %s", err)
	}
	switch strings.ToUpper(format) {
	// The distinction between this and PrintJSON is that this output is stripped of all pretty format.
	case outputFormatJSON:
		outputJSON, _ := json.Marshal(event)
		return fmt.Sprintln(ansi.ColorizeJSON(string(outputJSON), false, os.Stdout))
	default:
		return fmt.Sprintf("Unrecognized output format %s\n", format)
	}
}

//
// Public functions
//

// Init initializes a new Proxy
func Init(ctx context.Context, cfg *Config) (*Proxy, error) {
	if cfg.Log == nil {
		cfg.Log = &log.Logger{Out: io.Discard}
	}

	// validate forward-urls args
	if cfg.UseConfiguredWebhooks && len(cfg.ForwardURL) > 0 {
		if strings.HasPrefix(cfg.ForwardURL, "/") {
			return nil, errors.New("forward_to cannot be a relative path when loading webhook endpoints from the API")
		}
		if strings.HasPrefix(cfg.ForwardConnectURL, "/") {
			return nil, errors.New("forward_connect_to cannot be a relative path when loading webhook endpoints from the API")
		}
	} else if cfg.UseConfiguredWebhooks && len(cfg.ForwardURL) == 0 {
		return nil, errors.New("load_from_webhooks_api requires a location to forward to with forward_to")
	}

	// if no events are passed, listen for all events
	if len(cfg.Events) == 0 {
		cfg.Events = []string{"*"}
	} else {
		for _, event := range cfg.Events {
			if _, found := validEvents[event]; !found {
				cfg.Log.Warningf("You're attempting to listen for \"%s\", which isn't a valid event\n", event)
			}
		}
	}

	if len(cfg.ThinEvents) > 0 {
		for _, event := range cfg.ThinEvents {
			if _, found := validThinEvents[event]; !found {
				// If not found in validThinEvents, check in validPreviewThinEvents
				if _, foundInPreview := validPreviewThinEvents[event]; !foundInPreview {
					if event == "*" {
						cfg.Log.Infof("* is only supported in the CLI, thin event destinations do not support selecting all event types\n")
					} else {
						cfg.Log.Warningf("You're attempting to listen for \"%s\", which isn't a valid thin event or preview event\n", event)
					}
				}
			}
		}
	}

	// build from --forward-to urls if --forward-connect-to was not provided
	if len(cfg.ForwardConnectURL) == 0 {
		cfg.ForwardConnectURL = cfg.ForwardURL
	}
	if len(cfg.ForwardConnectHeaders) == 0 {
		cfg.ForwardConnectHeaders = cfg.ForwardHeaders
	}

	if len(cfg.ForwardThinConnectURL) == 0 {
		cfg.ForwardThinConnectURL = cfg.ForwardThinURL
	}

	// build endpoint routes
	var endpointRoutes []EndpointRoute
	if cfg.UseConfiguredWebhooks {
		// build from user's API config
		endpoints := getEndpointsFromAPI(ctx, cfg.Client)
		if len(endpoints.Data) == 0 {
			return nil, errors.New("you have not defined any webhook endpoints on your account, go to the Stripe Dashboard to add some: https://dashboard.stripe.com/test/webhooks")
		}
		var err error
		endpointRoutes, err = buildEndpointRoutes(endpoints, parseURL(cfg.ForwardURL), parseURL(cfg.ForwardConnectURL), cfg.ForwardHeaders, cfg.ForwardConnectHeaders)
		if err != nil {
			return nil, err
		}
	} else {
		if len(cfg.ForwardURL) > 0 {
			// non-connect endpoints
			endpointRoutes = append(endpointRoutes, EndpointRoute{
				URL:            parseURL(cfg.ForwardURL),
				ForwardHeaders: cfg.ForwardHeaders,
				Connect:        false,
				EventTypes:     cfg.Events,
			})
		}

		if len(cfg.ForwardConnectURL) > 0 {
			// connect endpoints
			endpointRoutes = append(endpointRoutes, EndpointRoute{
				URL:            parseURL(cfg.ForwardConnectURL),
				ForwardHeaders: cfg.ForwardConnectHeaders,
				Connect:        true,
				EventTypes:     cfg.Events,
			})
		}

		if len(cfg.ForwardThinURL) > 0 {
			// Thin endpoints
			endpointRoutes = append(endpointRoutes, EndpointRoute{
				URL:                parseURL(cfg.ForwardThinURL),
				ForwardHeaders:     cfg.ForwardHeaders,
				Connect:            false,
				EventTypes:         cfg.ThinEvents,
				IsEventDestination: true,
			})
		}

		if len(cfg.ForwardThinConnectURL) > 0 {
			// Thin connect endpoints
			endpointRoutes = append(endpointRoutes, EndpointRoute{
				URL:                parseURL(cfg.ForwardThinConnectURL),
				ForwardHeaders:     cfg.ForwardConnectHeaders,
				Connect:            true,
				EventTypes:         cfg.ThinEvents,
				IsEventDestination: true,
			})
		}
	}

	processorConfig := &WebhookEventProcessorConfig{
		Log:                          cfg.Log,
		Events:                       cfg.Events,
		ThinEvents:                   cfg.ThinEvents,
		OutCh:                        cfg.OutCh,
		UseLatestAPIVersion:          cfg.UseLatestAPIVersion,
		SkipVerify:                   cfg.SkipVerify,
		Timeout:                      cfg.Timeout,
		LoggedInAccountID:            cfg.LoggedInAccountID,
		OmitMarshaledPayload:         cfg.OmitMarshaledPayload,
		DisconnectOnMalformedPayload: cfg.DisconnectOnMalformedPayload,
		AcceptWebhookEvents:          slices.Contains(cfg.WebSocketFeatures, "webhooks"),
		AcceptStripeV2Events:         slices.Contains(cfg.WebSocketFeatures, "v2_events"),
	}

	p := &Proxy{
		cfg: cfg,
		stripeAuthClient: stripeauth.NewClient(cfg.Client, &stripeauth.Config{
			Log: cfg.Log,
		}),
	}
	p.webhookEventProcessor = NewWebhookEventProcessor(p.sendMessage, endpointRoutes, processorConfig)

	return p, nil
}

// IsValidEventType reports whether eventType is a supported snapshot event or
// the listen wildcard. Callers that use absence as evidence should fail closed
// on unknown filters instead of relying on Init's interactive warning.
func IsValidEventType(eventType string) bool {
	return eventType == "*" || validEvents[eventType]
}

// ExtractRequestData takes an interface with request data from a Stripe event payload
// and properly parses it into a StripeRequest struct before returning it
func ExtractRequestData(data interface{}) (StripeRequest, error) {
	switch v := data.(type) {
	// versions after 2017-05-25 represent request_data as an object
	case map[string]interface{}:
		req := StripeRequest{}

		if rawID, ok := v["id"]; ok && rawID != nil {
			id, ok := rawID.(string)
			if !ok {
				return StripeRequest{}, errors.New("received malformed event from Stripe")
			}
			req.ID = id
		}

		if rawKey, ok := v["idempotency_key"]; ok && rawKey != nil {
			key, ok := rawKey.(string)
			if !ok {
				return StripeRequest{}, errors.New("received malformed event from Stripe")
			}
			req.IdempotencyKey = key
		}

		return req, nil

	// versions including and prior to 2017-05-25 present the request field as
	// an optional string, which is nil when the event was triggered by a
	// non-user-visible request (e.g., 3D Secure callbacks).
	case string:
		return StripeRequest{ID: v}, nil
	case nil:
		return StripeRequest{}, nil
	}

	return StripeRequest{}, errors.New("received malformed event from Stripe")
}

//
// Private types
//

type eventContext struct {
	webhookID             string
	webhookConversationID string
	requestBody           string
	requestHeaders        map[string]string
	event                 *StripeEvent
	v2Event               *V2EventPayload
}

//
// Private constants
//

const (
	maxBodySize        = 5000
	maxNumHeaders      = 20
	maxHeaderKeySize   = 50
	maxHeaderValueSize = 200
)

const outputFormatJSON = "JSON"

//
// Private functions
//

// truncate will truncate str to be less than or equal to maxByteLength bytes.
// It will respect UTF8 and truncate the string at a code point boundary.
// If ellipsis is true, we'll append "..." to the truncated string if the string
// was in fact truncated, and if there's enough room. Note that the
// full string returned will always be <= maxByteLength bytes long, even with ellipsis.
func truncate(str string, maxByteLength int, ellipsis bool) string {
	if len(str) <= maxByteLength {
		return str
	}

	bytes := []byte(str)

	if ellipsis && maxByteLength > 3 {
		maxByteLength -= 3
	} else {
		ellipsis = false
	}

	for maxByteLength > 0 && maxByteLength < len(bytes) && isUTF8ContinuationByte(bytes[maxByteLength]) {
		maxByteLength--
	}

	result := string(bytes[0:maxByteLength])
	if ellipsis {
		result += "..."
	}

	return result
}

func isUTF8ContinuationByte(b byte) bool {
	return (b & 0xC0) == 0x80
}

// TODO: move to some helper somewhere
// parseURL parses the potentially incomplete URL provided in the configuration
// and returns a full URL
func parseURL(url string) string {
	_, err := strconv.Atoi(url)
	if err == nil {
		// If the input is just a number, assume it's a port number
		url = "localhost:" + url
	}

	if strings.HasPrefix(url, "/") {
		// If the input starts with a /, assume it's a relative path
		url = "localhost" + url
	}

	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		// Add the protocol if it's not already there
		url = "http://" + url
	}

	return url
}

func getEndpointsFromAPI(ctx context.Context, client stripe.RequestPerformer) requests.WebhookEndpointList {
	return requests.WebhookEndpointsListWithClient(ctx, client, stripe.APIVersion, &config.Profile{})
}

func buildEndpointRoutes(endpoints requests.WebhookEndpointList, forwardURL, forwardConnectURL string, forwardHeaders []string, forwardConnectHeaders []string) ([]EndpointRoute, error) {
	endpointRoutes := make([]EndpointRoute, 0)

	for _, endpoint := range endpoints.Data {
		// Ensure the endpoint is enabled.
		if endpoint.Status == "disabled" {
			continue
		}

		u, err := url.Parse(endpoint.URL)
		// Silently skip over invalid paths
		if err == nil {
			// Since webhooks in the dashboard may have a more generic url, only extract
			// the path. We'll use this with `localhost` or with the `--forward-to` flag
			if endpoint.Application == "" {
				url, err := buildForwardURL(forwardURL, u)
				if err != nil {
					return nil, err
				}
				endpointRoutes = append(endpointRoutes, EndpointRoute{
					URL:            url,
					ForwardHeaders: forwardHeaders,
					Connect:        false,
					EventTypes:     endpoint.EnabledEvents,
					Status:         endpoint.Status,
				})
			} else {
				url, err := buildForwardURL(forwardConnectURL, u)
				if err != nil {
					return nil, err
				}
				endpointRoutes = append(endpointRoutes, EndpointRoute{
					URL:            url,
					ForwardHeaders: forwardConnectHeaders,
					Connect:        true,
					EventTypes:     endpoint.EnabledEvents,
				})
			}
		}
	}

	return endpointRoutes, nil
}

func buildForwardURL(forwardURL string, destination *url.URL) (string, error) {
	f, err := url.Parse(forwardURL)
	if err != nil {
		return "", fmt.Errorf("provided forward url cannot be parsed: %s", forwardURL)
	}

	newForwardURL := fmt.Sprintf(
		"%s://%s%s%s",
		f.Scheme,
		f.Host,
		strings.TrimSuffix(f.Path, "/"), // avoids having a double "//"
		destination.Path,
	)

	if destination.RawQuery != "" {
		newForwardURL = newForwardURL + "?" + destination.RawQuery
	}

	return newForwardURL, nil
}

func getAPIVersionString(str *string) string {
	var APIVersion string

	if str == nil {
		APIVersion = "null"
	} else {
		APIVersion = *str
	}

	return APIVersion
}
