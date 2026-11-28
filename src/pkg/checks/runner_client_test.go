// SPDX-License-Identifier: Apache-2.0

package checks

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The default client must leave headroom above the longest scenario check
// timeout (120s in scenarios/mesh-traffic-management/scenario.yaml).
func TestNewRunner_DefaultHTTPClientTimeout(t *testing.T) {
	const longestScenarioCheck = 120 * time.Second
	got := NewRunner().HTTPClient.Timeout
	if got <= longestScenarioCheck {
		t.Errorf("default HTTP client timeout = %s, want more than %s (the longest scenario check timeout)", got, longestScenarioCheck)
	}
	if got != defaultHTTPTimeout {
		t.Errorf("default HTTP client timeout = %s, want %s", got, defaultHTTPTimeout)
	}
}

// A server that never answers must not hang a request made with a context
// that has no deadline. The default client ends it with a timeout error.
func TestNewRunner_DefaultHTTPClientEndsUnansweredRequest(t *testing.T) {
	const httpTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	client := newRunner(httpTimeout).HTTPClient
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("request to a server that never answers returned status %d, want a timeout error", resp.StatusCode)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("error = %v, want a timeout error", err)
	}
	if elapsed > httpTimeout+time.Second {
		t.Errorf("request ended after %s, want it to end within the client timeout %s", elapsed, httpTimeout)
	}
}
