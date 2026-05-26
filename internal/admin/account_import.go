package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

func decodeCreateAccountRequest(body []byte, defaultTenantID string) (createAccountReq, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return createAccountReq{}, errors.New("empty body")
	}
	req, err := normalizeAccountImportRequest(body, defaultTenantID)
	if err != nil {
		return createAccountReq{}, err
	}
	return req, nil
}

func decodeAccountImportRequests(body []byte, defaultTenantID string) ([]createAccountReq, bool, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || (body[0] != '[' && body[0] != '{') {
		return nil, false, nil
	}
	rawItems, ok, err := accountImportRawItems(body)
	if err != nil || !ok {
		return nil, ok, err
	}
	out := make([]createAccountReq, 0, len(rawItems))
	for _, raw := range rawItems {
		req, err := normalizeAccountImportRequest(raw, defaultTenantID)
		if err != nil {
			return nil, true, err
		}
		out = append(out, req)
	}
	return out, true, nil
}

func accountImportRawItems(body []byte) ([]json.RawMessage, bool, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(body, &arr); err == nil {
		return arr, true, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, false, nil
	}
	for _, key := range []string{"accounts", "Accounts"} {
		if raw, ok := obj[key]; ok {
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, true, errors.New("accounts must be an array")
			}
			return arr, true, nil
		}
	}
	return []json.RawMessage{append(json.RawMessage(nil), body...)}, true, nil
}

func normalizeAccountImportRequest(raw json.RawMessage, defaultTenantID string) (createAccountReq, error) {
	var req createAccountReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return req, err
	}

	if req.ID == "" {
		req.ID = firstImportString(obj, "id", "ID", "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId")
	}
	if req.TenantID == "" {
		req.TenantID = firstImportString(obj, "tenant_id", "tenantId", "TenantID")
	}
	if req.TenantID == "" {
		req.TenantID = defaultTenantID
	}
	if req.Email == "" {
		req.Email = firstImportString(obj, "email", "Email")
	}
	if req.PlanTier == "" {
		req.PlanTier = firstImportString(obj, "plan_tier", "planTier", "PlanTier", "plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType")
	}
	if req.RefreshToken == "" {
		req.RefreshToken = firstImportString(obj, "refresh_token", "refreshToken")
	}
	if req.Cookies == "" {
		req.Cookies = firstImportString(obj, "cookies", "Cookies")
	}

	if looksLikeCPAAuthJSON(obj) {
		if req.Provider == "" {
			req.Provider = "chatgpt"
		}
		req.SessionToken = compactRawJSON(raw)
	} else if req.Provider == "" {
		req.Provider = strings.ToLower(firstImportString(obj, "provider", "Provider"))
	}
	req.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
	return req, nil
}

func looksLikeCPAAuthJSON(obj map[string]any) bool {
	typ := strings.ToLower(firstImportString(obj, "type", "auth_mode", "authMode"))
	if typ == "codex" || typ == "chatgpt" {
		return true
	}
	if firstImportString(obj, "access_token", "accessToken", "id_token", "idToken") != "" {
		return true
	}
	for _, key := range []string{"token_data", "tokenData", "tokens", "metadata", "attributes"} {
		if nested, ok := obj[key].(map[string]any); ok && looksLikeCPAAuthJSON(nested) {
			return true
		}
	}
	if v, ok := obj["id_token_synthetic"].(bool); ok && v {
		return true
	}
	return false
}

func firstImportString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := importStringValue(obj[key]); value != "" {
			return value
		}
	}
	for _, nestedKey := range []string{"token_data", "tokenData", "tokens", "metadata", "attributes", "account"} {
		nested, ok := obj[nestedKey].(map[string]any)
		if !ok {
			continue
		}
		if value := firstImportString(nested, keys...); value != "" {
			return value
		}
	}
	return ""
}

func importStringValue(v any) string {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func compactRawJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}
