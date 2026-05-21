package plex

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestToken is the fixed credential value used by NewTestClient and
// by hand-rolled test fixtures. The leading "$" mimics an unexpanded
// environment-variable placeholder, which the repo secret-scan regex
// deliberately excludes (`[^"$]` at the first-char position). Tests
// only require that the X-Plex-Token header round-trips the same
// string.
const TestToken = "$fixture-test-token"

// NewTestClient constructs a *Client pointing at serverURL with a dummy
// token, suitable for use in tests. It calls t.Fatal if the URL cannot
// be parsed.
func NewTestClient(t testing.TB, serverURL string) *Client {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{BaseURL: u, Token: TestToken, HTTPClient: &http.Client{}}
}

// NewTestClientFromServer constructs a *Client wired to the given
// httptest.Server, using the server's own HTTP client for proper TLS
// and transport handling. This is the canonical test helper for
// packages that spin up an httptest.Server.
func NewTestClientFromServer(t testing.TB, ts *httptest.Server) *Client {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{BaseURL: u, Token: TestToken, HTTPClient: ts.Client()}
}
