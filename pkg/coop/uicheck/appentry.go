package uicheck

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// appEntryTimeout bounds the reachability probe. The app is normally on
// localhost, so a slow response means it is not really serving.
const appEntryTimeout = 5 * time.Second

// AppEntryProbe checks that the app page an agent reports as the start of a
// journey is actually being served. Without it an agent could satisfy the
// app-entry requirement with a plausible-looking URL for an app it never ran,
// which would put nothing on the verified path.
type AppEntryProbe struct {
	client *http.Client
}

// NewAppEntryProbe builds a probe with a bounded client that does not follow
// requests anywhere interesting: it only needs to know something answers.
func NewAppEntryProbe() *AppEntryProbe {
	return &AppEntryProbe{client: &http.Client{Timeout: appEntryTimeout}}
}

// ProbeAppEntry reports whether the URL is served. A refused connection or a
// server error means the developer would be sent to a dead page; a 404 means
// the specific entry page does not exist. Auth walls (401/403) and redirects
// are accepted — plenty of real app entry points sit behind a login.
func (p *AppEntryProbe) ProbeAppEntry(ctx context.Context, rawURL string) error {
	ctx, cancel := context.WithTimeout(ctx, appEntryTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("%s is not a usable URL", rawURL)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("nothing is serving %s — start your app first", rawURL)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	switch {
	case response.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s returns 404 — point at the page that actually starts the journey", rawURL)
	case response.StatusCode >= 500:
		return fmt.Errorf("%s returns %d — the app is erroring on the page the developer would open", rawURL, response.StatusCode)
	}
	return nil
}
