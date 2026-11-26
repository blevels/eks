// Package firecall implements the wire contract visible in the POC.
package firecall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	HTTP                         *http.Client
	OAuthURL, ListURL, CreateURL string
}

// New disallows redirects: neither OAuth secrets nor bearer tokens should be
// forwarded to a different endpoint, including HTTPS-to-HTTP redirects.
func New(timeout time.Duration, oauth, list, create string) *Client {
	return &Client{HTTP: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}, OAuthURL: oauth, ListURL: list, CreateURL: create}
}

type Active struct {
	Requester string `json:"requester"`
	Role      string `json:"iamRoleArn"`
	EndTime   string `json:"end_time"`
	Status    string `json:"firecall_status"`
	Account   string `json:"account_id"`
}

type Created struct {
	ID      string `json:"firecall_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// UnmarshalJSON preserves jq's ability to display string or numeric request IDs
// without decoding large integer IDs through float64.
func (c *Created) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID      json.RawMessage `json:"firecall_id"`
		Status  string          `json:"status"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	c.Status, c.Message = wire.Status, wire.Message
	c.ID = ""
	if len(wire.ID) == 0 || string(wire.ID) == "null" {
		return nil
	}
	if err := json.Unmarshal(wire.ID, &c.ID); err == nil {
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(wire.ID, &number); err != nil {
		return errors.New("firecall_id must be a string or number")
	}
	c.ID = number.String()
	return nil
}

func (c *Client) request(ctx context.Context, method, endpoint, token, contentType string, body io.Reader, out any) (err error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("construct API request: invalid endpoint")
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Do not include URLs, credential-bearing headers, or response bodies.
		if ctx.Err() != nil {
			return fmt.Errorf("API request cancelled: %w", ctx.Err())
		}
		return errors.New("API request failed (network, TLS, or timeout)")
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close API response: %w", closeErr))
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil {
		return fmt.Errorf("read API response: %w", err)
	}
	if len(data) > 1024*1024 {
		return errors.New("API response exceeds 1 MiB")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("API returned invalid or unexpected JSON")
	}
	return nil
}

func (c *Client) Token(ctx context.Context, username, password string) (string, error) {
	form := url.Values{"client_id": {username}, "client_secret": {password}, "grant_type": {"client_credentials"}, "scope": {"AppIdClaimsTrust"}}
	var response struct {
		Token string `json:"access_token"`
	}
	if err := c.request(ctx, http.MethodPost, c.OAuthURL, "", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), &response); err != nil {
		return "", fmt.Errorf("fetch OAuth token: %w", err)
	}
	if strings.TrimSpace(response.Token) == "" || response.Token == "null" {
		return "", errors.New("OAuth response missing access_token")
	}
	if strings.ContainsAny(response.Token, "\r\n") {
		return "", errors.New("OAuth response contains an invalid token")
	}
	return response.Token, nil
}

func (c *Client) List(ctx context.Context, token, account string) ([]Active, error) {
	var all []Active
	if err := c.request(ctx, http.MethodGet, c.ListURL, token, "", nil, &all); err != nil {
		return nil, fmt.Errorf("list firecalls: %w", err)
	}
	found := []Active{}
	// Preserve the POC's account + active-status matching policy. Do not add
	// requester, role, or timestamp-based authorization rules silently.
	for _, item := range all {
		if item.Status == "active" && item.Account == account {
			found = append(found, item)
		}
	}
	return found, nil
}

func (c *Client) Create(ctx context.Context, token, arn, email, ticket string, duration int) ([]Created, error) {
	if strings.TrimSpace(ticket) == "" {
		return nil, errors.New("a change/incident ticket is required")
	}
	body, err := json.Marshal(struct {
		Roles    []string `json:"iam_role_arn"`
		Duration string   `json:"duration_in_hours"`
		Ticket   string   `json:"ticket"`
		Email    string   `json:"email"`
	}{[]string{arn}, fmt.Sprint(duration), ticket, email})
	if err != nil {
		return nil, fmt.Errorf("encode firecall request: %w", err)
	}
	var all []Created
	if err := c.request(ctx, http.MethodPost, c.CreateURL, token, "application/json", strings.NewReader(string(body)), &all); err != nil {
		return nil, fmt.Errorf("create firecall (not retried; verify request status before retrying): %w", err)
	}
	success := []Created{}
	for _, item := range all {
		if item.Status == "SUCCESS" && item.ID != "" && item.ID != "null" {
			success = append(success, item)
		}
	}
	if len(success) == 0 {
		return nil, errors.New("firecall response did not report a successful request ID")
	}
	return success, nil
}
