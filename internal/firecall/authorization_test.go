package firecall

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListRejectsApprovalsFromFailedResponses(t *testing.T) {
	const account = "123456789012"
	const arn = "arn:aws:iam::123456789012:role/path/Role"
	const approval = `{"firecall_status":"active","account_id":"123456789012","iamRoleArn":"arn:aws:iam::123456789012:role/path/Role"}`
	for _, tt := range []struct {
		name     string
		status   int
		body     string
		category ErrorCategory
	}{
		{
			name:     "invalid field type after matching approval",
			status:   http.StatusOK,
			body:     `[` + approval + `,{"firecall_status":42}]`,
			category: CategoryInvalidJSON,
		},
		{
			name:     "error status with matching approval",
			status:   http.StatusForbidden,
			body:     `[` + approval + `]`,
			category: CategoryHTTP,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				if _, err := w.Write([]byte(tt.body)); err != nil {
					t.Error(err)
				}
			}))
			defer s.Close()
			c := New(time.Second, "", s.URL, "")
			found, err := c.List(context.Background(), "token", account, arn)
			var failure *RequestError
			if !errors.As(err, &failure) || failure.Category != tt.category || failure.StatusCode != tt.status {
				t.Fatalf("expected %s error with HTTP %d, got %v", tt.category, tt.status, err)
			}
			if len(found) != 0 {
				t.Fatalf("failed response returned active approvals: %+v", found)
			}
		})
	}
}

func TestListRequiresActiveAccountAndFullRoleARN(t *testing.T) {
	const account = "123456789012"
	const arn = "arn:aws:iam::123456789012:role/path/Role"
	for _, tt := range []struct {
		name string
		item Active
		want int
	}{
		{"exact", Active{Status: "active", Account: account, Role: arn}, 1},
		{"different requester and unparsed expiry", Active{Status: "active", Account: account, Role: arn, Requester: "other", EndTime: "not-a-time"}, 1},
		{"pending", Active{Status: "pending", Account: account, Role: arn}, 0},
		{"status case", Active{Status: "ACTIVE", Account: account, Role: arn}, 0},
		{"other account", Active{Status: "active", Account: "000000000000", Role: arn}, 0},
		{"other role", Active{Status: "active", Account: account, Role: "arn:aws:iam::123456789012:role/Other"}, 0},
		{"role name only", Active{Status: "active", Account: account, Role: "path/Role"}, 0},
		{"missing role", Active{Status: "active", Account: account}, 0},
		{"role case", Active{Status: "active", Account: account, Role: "arn:aws:iam::123456789012:role/path/role"}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if err := json.NewEncoder(w).Encode([]Active{tt.item}); err != nil {
					t.Error(err)
				}
			}))
			defer s.Close()
			c := New(time.Second, "", s.URL, "")
			found, err := c.List(context.Background(), "token", account, arn)
			if err != nil || len(found) != tt.want {
				t.Fatalf("found=%v err=%v", found, err)
			}
		})
	}
}
