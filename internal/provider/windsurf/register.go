package windsurf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// RegisterUser exchanges a firebase ID token for a long-lived Windsurf API key.
func RegisterUser(ctx context.Context, firebaseToken string) (apiKey string, err error) {
	endpoints := []string{
		"https://register.windsurf.com/exa.seat_management_pb.SeatManagementService/RegisterUser",
		"https://api.codeium.com/register_user/",
	}

	body, _ := json.Marshal(map[string]string{
		"firebase_id_token": firebaseToken,
	})

	client := &http.Client{Timeout: 30 * time.Second}

	for _, ep := range endpoints {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "windsurf/1.9600.41")

		resp, e := client.Do(req)
		if e != nil {
			err = e
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			err = fmt.Errorf("register %s: status %d: %s", ep, resp.StatusCode, string(respBody))
			continue
		}

		var result map[string]any
		if e := json.Unmarshal(respBody, &result); e != nil {
			err = fmt.Errorf("register: parse response: %w", e)
			continue
		}

		// api_key or apiKey
		if k, ok := result["api_key"].(string); ok && k != "" {
			return k, nil
		}
		if k, ok := result["apiKey"].(string); ok && k != "" {
			return k, nil
		}
		err = fmt.Errorf("register: no api_key in response: %s", string(respBody))
	}
	return "", err
}
