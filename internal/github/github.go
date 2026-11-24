// Package github opens and tracks drift pull requests.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one repository.
type Client struct {
	Repo  string // owner/name
	Token string
	API   string // defaults to https://api.github.com
	HTTP  *http.Client
}

// PR is a pull request.
type PR struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	URL    string `json:"html_url"`
}

// EnsurePR opens a PR, or updates the open one.
func (c *Client) EnsurePR(ctx context.Context, head, base, title, body string) (PR, error) {
	owner, _, _ := strings.Cut(c.Repo, "/")
	var open []PR
	q := url.Values{"state": {"open"}, "head": {owner + ":" + head}, "base": {base}}
	if err := c.do(ctx, http.MethodGet, "/repos/"+c.Repo+"/pulls?"+q.Encode(), nil, &open); err != nil {
		return PR{}, err
	}
	payload := map[string]any{"title": title, "body": body}
	var pr PR
	if len(open) > 0 {
		err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, open[0].Number), payload, &pr)
		return pr, err
	}
	payload["head"], payload["base"] = head, base
	err := c.do(ctx, http.MethodPost, "/repos/"+c.Repo+"/pulls", payload, &pr)
	return pr, err
}

// Get fetches a pull request.
func (c *Client) Get(ctx context.Context, n int) (PR, error) {
	var pr PR
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, n), nil, &pr)
	return pr, err
}

// Comment adds a comment to a pull request.
func (c *Client) Comment(ctx context.Context, n int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", c.Repo, n), map[string]any{"body": body}, nil)
}

// Close comments on and closes a pull request.
func (c *Client) Close(ctx context.Context, n int, comment string) error {
	if comment != "" {
		if err := c.Comment(ctx, n, comment); err != nil {
			return err
		}
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, n), map[string]any{"state": "closed"}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	api := c.API
	if api == "" {
		api = "https://api.github.com"
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(api, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
