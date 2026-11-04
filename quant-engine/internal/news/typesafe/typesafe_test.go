package typesafe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
)

const orKey = "or-canary-key-5f2a"

// The body of an OpenRouter answer, extra fields included; a model id in the
// form the documentation shows.
const orAnswer = `{"id":"gen-1","provider":"TypeSafe","model":"typesafe/jev-1.13-20260917",` +
	`"answers":{"catastrophe":{"type":"noul","noul":0.42}},"usage":{"input_tokens":12,"output_tokens":0,"cost":0.0001}}`

func orClient(url string) *Client {
	return &Client{URL: url, Name: "openrouter", Key: config.Secret(orKey), KeyVar: "S_OPENROUTER_API_KEY",
		HTTP: http.DefaultClient, Backoff: time.Millisecond}
}

var orQuestion = news.Question{Type: "noul", Instructions: "Catastrophe?", Model: "jev-latest"}

func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// The endpoint is the one OpenRouter documents, not a value any config gives.
func TestOpenRouterEndpoint(t *testing.T) {
	if OpenRouterEndpoint != "https://openrouter.ai/api/v1/systemone" {
		t.Fatalf("endpoint %q", OpenRouterEndpoint)
	}
}

// OpenRouter's answer is read as TypeSafe's: the extra fields pass, and the
// model comes through as given, whatever its form.
func TestOpenRouterAnswerIsRead(t *testing.T) {
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+orKey {
			t.Errorf("authorization header is not the bearer key")
		}
		_, _ = io.WriteString(w, orAnswer)
	})
	v, err := orClient(url).Classify(context.Background(), "state", orQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if v.Model != "typesafe/jev-1.13-20260917" || v.Yes != 0.42 {
		t.Fatalf("verdict %+v", v)
	}
}

// Every failing status OpenRouter documents is an error naming the provider,
// asked once and without the key, even when the body echoes it. 402 (no
// credit) above all: waiting does not bring credit back.
func TestOpenRouterFailuresAreErrors(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 408, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			url := serve(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, `{"error":{"code":`+fmt.Sprint(status)+`,"message":"bad key `+orKey+`"}}`, status)
			})
			_, err := orClient(url).Classify(context.Background(), "state", orQuestion)
			want := fmt.Sprintf("openrouter: HTTP %d", status)
			if err == nil || err.Error() != want {
				t.Fatalf("error %v; want %q", err, want)
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("%d requests; want 1", n)
			}
		})
	}
}

// A 429 is retried at OpenRouter as at TypeSafe.
func TestOpenRouterRateLimitIsRetried(t *testing.T) {
	var calls atomic.Int32
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, orAnswer)
	})
	if _, err := orClient(url).Classify(context.Background(), "state", orQuestion); err != nil || calls.Load() != 2 {
		t.Fatalf("err %v after %d requests; want the second to pass", err, calls.Load())
	}
}

// A 200 carrying an error instead of answers has no verdict.
func TestOpenRouterErrorInA200IsAnError(t *testing.T) {
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"error":{"code":502,"message":"upstream `+orKey+`"}}`)
	})
	_, err := orClient(url).Classify(context.Background(), "state", orQuestion)
	if err == nil || strings.Contains(err.Error(), orKey) || !strings.HasPrefix(err.Error(), "openrouter: ") {
		t.Fatalf("error %v", err)
	}
}

// Without a key no request is sent, and the error names the variable.
func TestOpenRouterMissingKeyNamesItsVariable(t *testing.T) {
	c := orClient("http://127.0.0.1:0")
	c.Key = ""
	_, err := c.Classify(context.Background(), "state", orQuestion)
	if err == nil || err.Error() != "openrouter: environment variable S_OPENROUTER_API_KEY is not set" {
		t.Fatalf("error %v", err)
	}
}

// With no Name, the errors read as they always did.
func TestDefaultNameIsTypeSafe(t *testing.T) {
	url := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 401) })
	c := orClient(url)
	c.Name = ""
	if _, err := c.Classify(context.Background(), "state", orQuestion); err == nil || err.Error() != "typesafe: HTTP 401" {
		t.Fatalf("error %v", err)
	}
}
