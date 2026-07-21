package uicheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statusServer serves the given status on every path, and records the method
// so the probe stays a cheap read.
func statusServer(t *testing.T, status int) (*httptest.Server, *[]string) {
	t.Helper()
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, &methods
}

// --- 1. What the probe accepts ---

func TestProbeAppEntryAcceptsServedPages(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "ok", status: http.StatusOK},
		{name: "no content", status: http.StatusNoContent},
		// Redirects are followed by the client; a login wall answering 401/403
		// still proves something is serving the page.
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden},
		// Anything short of a server error is the app's business, not ours.
		{name: "bad request", status: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, methods := statusServer(t, tt.status)

			require.NoError(t, NewAppEntryProbe().ProbeAppEntry(context.Background(), server.URL+"/cart"))
			assert.Equal(t, []string{http.MethodGet}, *methods)
		})
	}
}

func TestProbeAppEntryFollowsRedirectToALoginWall(t *testing.T) {
	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(login.Close)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, login.URL, http.StatusFound)
	}))
	t.Cleanup(app.Close)

	require.NoError(t, NewAppEntryProbe().ProbeAppEntry(context.Background(), app.URL+"/cart"))
}

// --- 2. What the probe refuses ---

func TestProbeAppEntryRejectsMissingPage(t *testing.T) {
	server, _ := statusServer(t, http.StatusNotFound)

	err := NewAppEntryProbe().ProbeAppEntry(context.Background(), server.URL+"/cart")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
	assert.Contains(t, err.Error(), server.URL+"/cart")
}

func TestProbeAppEntryRejectsServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway} {
		server, _ := statusServer(t, status)

		err := NewAppEntryProbe().ProbeAppEntry(context.Background(), server.URL+"/cart")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the app is erroring")
	}
}

// Nothing listening is the case the probe exists for: it is what an agent
// reporting a plausible URL for an app it never ran would produce.
func TestProbeAppEntryRejectsDeadAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := server.URL + "/cart"
	server.Close()

	err := NewAppEntryProbe().ProbeAppEntry(context.Background(), dead)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing is serving")
	assert.Contains(t, err.Error(), dead)
	assert.Contains(t, err.Error(), "start your app first")
}

func TestProbeAppEntryRejectsUnusableURL(t *testing.T) {
	err := NewAppEntryProbe().ProbeAppEntry(context.Background(), "http://\x7f/cart")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a usable URL")
}

// A caller-cancelled context must not be reported as a usable page.
func TestProbeAppEntryHonorsCallerCancellation(t *testing.T) {
	server, _ := statusServer(t, http.StatusOK)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := NewAppEntryProbe().ProbeAppEntry(ctx, server.URL+"/cart")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing is serving")
}
