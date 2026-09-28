package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// RefreshTokenError classifies a token-endpoint rejection without retaining its
// response body, which may contain credentials or other sensitive data.
type RefreshTokenError struct {
	StatusCode int
	Permanent  bool
	Code       string
}

func (e *RefreshTokenError) Error() string {
	if e.Permanent {
		return "OpenAI OAuth credentials permanently rejected; sign in again"
	}
	return fmt.Sprintf("OpenAI OAuth token refresh temporarily failed (status %d)", e.StatusCode)
}

// NewRefreshTokenError follows codex-login's token-endpoint classification.
// Only recognized codes are retained; arbitrary upstream strings are not safe
// to put in account state, logs, or client-facing error messages.
func NewRefreshTokenError(status int, body []byte) *RefreshTokenError {
	var response struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	code := ""
	if json.Unmarshal(body, &response) == nil {
		var nested struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(response.Error, &nested) == nil && nested.Code != "" {
			code = nested.Code
		} else if json.Unmarshal(response.Error, &code) != nil || code == "" {
			code = response.Code
		}
	}
	err := &RefreshTokenError{StatusCode: status, Permanent: status == http.StatusUnauthorized}
	switch strings.ToLower(code) {
	case "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		err.Code = strings.ToLower(code)
		err.Permanent = true
	case "invalid_grant":
		err.Code = "invalid_grant"
		err.Permanent = err.Permanent || status == http.StatusBadRequest
	case "invalid_client", "unauthorized_client", "invalid_scope":
		err.Code = strings.ToLower(code)
	}
	return err
}
