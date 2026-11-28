package firecall

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWireContract(t *testing.T) {
	var calls []string
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/oauth":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("client_id") != "suser" || r.Form.Get("client_secret") != "a&b=+?" || r.Form.Get("scope") != "AppIdClaimsTrust" || r.Form.Get("grant_type") != "client_credentials" {
				t.Errorf("bad form: %v", r.Form)
			}
			fmt.Fprint(w, `{ "access_token" : "token" }`)
		case "/list":
			if r.Header.Get("Authorization") != "Bearer token" {
				t.Error("missing bearer")
			}
			fmt.Fprint(w, `[
				{"firecall_status":"active","account_id":"123456789012","requester":"another-user","iamRoleArn":"arn:test","end_time":"not-parsed"},
				{"firecall_status":"active","account_id":"123456789012","iamRoleArn":"different-role"},
				{"firecall_status":"active","account_id":"123456789012"},
				{"firecall_status":"closed","account_id":"123456789012"},
				{"firecall_status":"active","account_id":"000000000000"}
			]`)
		case "/create":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["duration_in_hours"] != "8" || body["email"] != "user@test.com" || body["ticket"] != "CHG1" {
				t.Errorf("bad create body: %v", body)
			}
			roles, ok := body["iam_role_arn"].([]any)
			if !ok || len(roles) != 1 || roles[0] != "arn:test" {
				t.Error("invalid role array", roles)
			}
			fmt.Fprint(w, `[{"firecall_id":"42","status":"SUCCESS","message":"created"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c := New(time.Second, s.URL+"/oauth", s.URL+"/list", s.URL+"/create")
	c.HTTP = s.Client()
	token, err := c.Token(context.Background(), "suser", "a&b=+?")
	if err != nil || token != "token" {
		t.Fatal(token, err)
	}
	active, err := c.List(context.Background(), token, "123456789012", "arn:test")
	if err != nil || len(active) != 1 || active[0].Requester != "another-user" {
		t.Fatal(active, err)
	}
	created, err := c.Create(context.Background(), token, "arn:test", "user@test.com", "CHG1", 8)
	if err != nil || len(created) != 1 || created[0].ID != "42" {
		t.Fatal(created, err)
	}
	if len(calls) != 3 {
		t.Fatal(calls)
	}
}

func TestTokenFailures(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
	}{
		{"missing", `{}`, 200}, {"null", `{"access_token":null}`, 200},
		{"empty", `{"access_token":""}`, 200}, {"wrong type", `{"access_token":17}`, 200},
		{"malformed", `{`, 200}, {"server", "do-not-log-this", 500},
		{"unauthorized with token", `{"access_token":"do-not-log-this"}`, http.StatusUnauthorized},
		{"server error with token", `{"access_token":"do-not-log-this"}`, http.StatusInternalServerError},
		{"unavailable with token", `{"access_token":"do-not-log-this"}`, http.StatusServiceUnavailable},
		{"oversize", strings.Repeat("x", 1024*1024+1), 200},
		{"header injection", `{"access_token":"x\r\ny"}`, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer s.Close()
			c := New(time.Second, s.URL, "", "")
			token, err := c.Token(context.Background(), "u", "secret")
			if err == nil || strings.Contains(err.Error(), "do-not-log-this") {
				t.Fatal(err)
			}
			if token != "" {
				t.Fatal("failed OAuth response returned a nonempty token")
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1 (POST must not be retried)", got)
			}
		})
	}
}

func TestRedirectAndCancellation(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var originCalls, destinationCalls atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				destinationCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"access_token":"destination-token"}`)
			}))
			defer destination.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originCalls.Add(1)
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("client_id") != "u" || r.Form.Get("client_secret") != "secret" {
					t.Error("origin did not receive OAuth credentials")
				}
				w.Header().Set("Location", destination.URL)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"access_token":"origin-token"}`)
			}))
			defer origin.Close()
			c := New(time.Second, origin.URL, "", "")
			token, err := c.Token(context.Background(), "u", "secret")
			if err == nil {
				t.Error("redirect accepted")
			}
			if token != "" {
				t.Error("redirect returned a nonempty token")
			}
			if got := originCalls.Load(); got != 1 {
				t.Errorf("origin requests = %d, want 1 (POST must not be retried)", got)
			}
			if got := destinationCalls.Load(); got != 0 {
				t.Errorf("destination requests = %d, want 0 (credentials must not be forwarded)", got)
			}
		})
	}
	t.Run("cancellation", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) }))
		defer s.Close()
		c := New(time.Second, s.URL, "", "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := c.Token(ctx, "u", "p"); err == nil {
			t.Fatal("cancelled request accepted")
		}
	})
}

func TestCreateRejectsUnsuccessfulResponses(t *testing.T) {
	for _, body := range []string{`[]`, `[{"firecall_id":"42","status":"PENDING"}]`, `[{"status":"SUCCESS"}]`, `[{"firecall_id":"null","status":"SUCCESS"}]`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer s.Close()
			c := New(time.Second, "", "", s.URL)
			if _, err := c.Create(context.Background(), "token", "arn", "email", "ticket", 8); err == nil {
				t.Fatal("unsuccessful creation accepted")
			}
		})
	}
	c := New(time.Second, "", "", "")
	if _, err := c.Create(context.Background(), "", "", "", "", 8); err == nil {
		t.Fatal("empty ticket accepted")
	}
}

func TestCreatedIDTypes(t *testing.T) {
	for _, tt := range []struct {
		body, want string
		bad        bool
	}{
		{`{"firecall_id":"abc"}`, "abc", false},
		{`{"firecall_id":1234567890123456789}`, "1234567890123456789", false},
		{`{"firecall_id":null}`, "", false},
		{`{"firecall_id":true}`, "", true},
	} {
		var item Created
		err := json.Unmarshal([]byte(tt.body), &item)
		if (err != nil) != tt.bad || item.ID != tt.want {
			t.Fatal(item, err)
		}
	}
}

func TestListAndHTTPTimeout(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	defer s.Close()
	c := New(10*time.Millisecond, "", s.URL, s.URL)
	found, err := c.List(context.Background(), "t", "account", "arn:test")
	if err != nil || len(found) != 0 {
		t.Fatal(found, err)
	}
	c.OAuthURL = s.URL + "/slow"
	if _, err := c.Token(context.Background(), "u", "p"); err == nil {
		t.Fatal("timeout ignored")
	}
}
