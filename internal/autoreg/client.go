package autoreg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

type PyAccount struct {
	ID              int                    `json:"id"`
	Platform        string                 `json:"platform"`
	Email           string                 `json:"email"`
	Password        string                 `json:"password"`
	UserID          string                 `json:"user_id"`
	PrimaryToken    string                 `json:"primary_token"`
	LifecycleStatus string                 `json:"lifecycle_status"`
	ValidityStatus  string                 `json:"validity_status"`
	PlanName        string                 `json:"plan_name"`
	Credentials     []PyCredential         `json:"credentials"`
	Overview        map[string]interface{} `json:"overview"`
	CreatedAt       *string                `json:"created_at"`
}

type PyCredential struct {
	Scope          string `json:"scope"`
	Key            string `json:"key"`
	Value          string `json:"value"`
	CredentialType string `json:"credential_type"`
	IsPrimary      bool   `json:"is_primary"`
}

type AccountListResponse struct {
	Total int         `json:"total"`
	Page  int         `json:"page"`
	Items []PyAccount `json:"items"`
}

func (c *Client) ListUnsyncedAccounts(ctx context.Context, page, pageSize int) (*AccountListResponse, error) {
	url := fmt.Sprintf("%s/api/accounts?synced=false&page=%d&page_size=%d", c.baseURL, page, pageSize)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list unsynced: status %d: %s", resp.StatusCode, body)
	}
	var result AccountListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) MarkSynced(ctx context.Context, accountID int) error {
	url := fmt.Sprintf("%s/api/accounts/%d/synced", c.baseURL, accountID)
	req, err := http.NewRequestWithContext(ctx, "PATCH", url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("mark synced %d: status %d", accountID, resp.StatusCode)
	}
	return nil
}

func (c *Client) Health(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/health", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("health: status %d", resp.StatusCode)
	}
	return nil
}
