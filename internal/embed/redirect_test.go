package embed

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelAPIRedirectPolicy(t *testing.T) {
	for _, scenario := range []string{"same_origin", "other_port", "https_to_http", "caller_policy"} {
		t.Run(scenario, func(t *testing.T) {
			var received, callerCalls atomic.Int32
			var destination string
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/finish" {
					http.Redirect(w, r, destination, http.StatusTemporaryRedirect)
					return
				}
				received.Add(1)
				if r.Header.Get("Authorization") != "Bearer model-secret" {
					t.Errorf("same-origin redirect lost Authorization: %q", r.Header.Get("Authorization"))
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || !strings.Contains(string(body), "private memory") {
					t.Errorf("same-origin redirect lost request body: %q, %v", body, err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,2,3,4]}]}`)
			})
			var source *httptest.Server
			if scenario == "https_to_http" {
				source = httptest.NewTLSServer(handler)
			} else {
				source = httptest.NewServer(handler)
			}
			defer source.Close()
			destination = source.URL + "/finish"
			if scenario == "other_port" {
				target := httptest.NewServer(handler)
				defer target.Close()
				destination = target.URL + "/finish"
			}
			if scenario == "https_to_http" {
				// Keep the host and port unchanged so this independently
				// verifies the HTTPS downgrade check.
				destination = strings.Replace(source.URL, "https://", "http://", 1) + "/finish"
			}
			hc := source.Client()
			hc.Timeout = 5 * time.Second
			hc.CheckRedirect = func(*http.Request, []*http.Request) error {
				callerCalls.Add(1)
				if scenario == "caller_policy" {
					return errors.New("caller refused")
				}
				return nil
			}
			c, err := New(Config{BaseURL: source.URL + "/start", Model: "m", Dim: dim, APIKey: "model-secret", HTTPClient: hc})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Embed(context.Background(), []string{"private memory"})
			switch scenario {
			case "same_origin":
				if err != nil || received.Load() != 1 || callerCalls.Load() != 1 {
					t.Errorf("same-origin request: received %d, caller checks %d, error %v", received.Load(), callerCalls.Load(), err)
				}
			case "caller_policy":
				if err == nil || !strings.Contains(err.Error(), "caller refused") || received.Load() != 0 || callerCalls.Load() != 1 {
					t.Errorf("caller redirect policy was not preserved: received %d, caller checks %d, error %v", received.Load(), callerCalls.Load(), err)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), "different origin") || received.Load() != 0 || callerCalls.Load() != 0 {
					t.Errorf("unsafe redirect was not refused: received %d, caller checks %d, error %v", received.Load(), callerCalls.Load(), err)
				}
			}
		})
	}
}
