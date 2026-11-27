// Package firecall implements the wire contract visible in the POC.
package firecall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	HTTP                         *http.Client
	OAuthURL, ListURL, CreateURL string
	// Log is optional. Configure it before sharing the client between callers.
	Log *slog.Logger

	timeout time.Duration
	wait    func(context.Context, time.Duration) error
	jitter  func(time.Duration) time.Duration
}

// New disallows redirects: neither OAuth secrets nor bearer tokens should be
// forwarded to a different endpoint, including HTTPS-to-HTTP redirects.
func New(timeout time.Duration, oauth, list, create string) *Client {
	return &Client{HTTP: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}, OAuthURL: oauth, ListURL: list, CreateURL: create, timeout: timeout}
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

func (c *Client) Token(ctx context.Context, username, password string) (string, error) {
	form := url.Values{"client_id": {username}, "client_secret": {password}, "grant_type": {"client_credentials"}, "scope": {"AppIdClaimsTrust"}}
	var response struct {
		Token string `json:"access_token"`
	}
	if err := c.request(ctx, opToken, http.MethodPost, c.OAuthURL, "", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), &response); err != nil {
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

func (c *Client) List(ctx context.Context, token, account, roleARN string) ([]Active, error) {
	var all []Active
	if err := c.request(ctx, opList, http.MethodGet, c.ListURL, token, "", nil, &all); err != nil {
		return nil, fmt.Errorf("list firecalls: %w", err)
	}
	found := []Active{}
	// Authorization requires the selected account and full IAM role ARN.
	// Requester and expiration checks are intentionally not part of this policy.
	for _, item := range all {
		if item.Status == "active" && item.Account == account && item.Role == roleARN {
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
	if err := c.request(ctx, opCreate, http.MethodPost, c.CreateURL, token, "application/json", strings.NewReader(string(body)), &all); err != nil {
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
