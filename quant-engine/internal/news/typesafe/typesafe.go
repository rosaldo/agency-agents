// Package typesafe is the news.Classifier on TypeSafe's Jev model: one
// noul question per headline, POST /v1/systemone, standard library HTTP.
// The API key is a config.Secret: it goes in the Authorization header only,
// never in an error, a log line or the journal.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"

	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
)

// Endpoint is TypeSafe's classification endpoint.
const Endpoint = "https://api.typesafe.ai/v1/systemone"

const questionID = "catastrophe"

type Client struct {
	URL    string        // Endpoint, or a test server
	Key    config.Secret // empty: every call fails, naming KeyVar
	KeyVar string        // the environment variable the key comes from
	HTTP   *http.Client
}

type question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

type request struct {
	State     string              `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]question `json:"questions"`
}

type response struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Type string   `json:"type"`
		Noul *float64 `json:"noul"`
	} `json:"answers"`
}

// Classify asks q about the headline state.
func (c *Client) Classify(ctx context.Context, state string, q news.Question) (news.Verdict, error) {
	if c.Key == "" {
		return news.Verdict{}, fmt.Errorf("typesafe: environment variable %s is not set", c.KeyVar)
	}
	body, err := json.Marshal(request{State: state, Model: q.Model,
		Questions: map[string]question{questionID: {Type: q.Type, Instructions: q.Instructions, Criteria: q.Criteria}}})
	if err != nil {
		return news.Verdict{}, fmt.Errorf("typesafe: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return news.Verdict{}, fmt.Errorf("typesafe: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key.Reveal())
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if ue := (*neturl.Error)(nil); errors.As(err, &ue) {
		err = fmt.Errorf("%s: %w", ue.Op, ue.Err) // the URL is ours; nothing of the request is repeated
	}
	if err != nil {
		return news.Verdict{}, fmt.Errorf("typesafe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return news.Verdict{}, fmt.Errorf("typesafe: HTTP %d", resp.StatusCode) // 429 rate limit, 529 overloaded, ...
	}
	var r response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return news.Verdict{}, fmt.Errorf("typesafe: unreadable answer: %w", err)
	}
	a, ok := r.Answers[questionID]
	if !ok || a.Noul == nil || *a.Noul < 0 || *a.Noul > 1 {
		return news.Verdict{}, errors.New("typesafe: the answer has no noul probability for the question")
	}
	return news.Verdict{Model: r.Model, Yes: *a.Noul}, nil
}
