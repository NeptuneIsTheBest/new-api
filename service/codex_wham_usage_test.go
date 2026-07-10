package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func codexWhamTestClient(t *testing.T, handler func(*http.Request) string) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := handler(r)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		}),
	}
}

func TestFetchCodexWhamRateLimitResetCreditsForwardsRequest(t *testing.T) {
	client := codexWhamTestClient(t, func(r *http.Request) string {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/backend-api/wham/rate-limit-reset-credits", r.URL.Path)
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		require.Equal(t, "account-123", r.Header.Get("chatgpt-account-id"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		require.Equal(t, "codex_cli_rs", r.Header.Get("originator"))
		return `{"available_count":1}`
	})

	statusCode, body, err := FetchCodexWhamRateLimitResetCredits(context.Background(), client, "https://upstream.example/", "access-token", "account-123")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, statusCode)
	require.JSONEq(t, `{"available_count":1}`, string(body))
}

func TestConsumeCodexWhamRateLimitResetCreditForwardsJSONBody(t *testing.T) {
	client := codexWhamTestClient(t, func(r *http.Request) string {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/backend-api/wham/rate-limit-reset-credits/consume", r.URL.Path)
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		require.Equal(t, "account-123", r.Header.Get("chatgpt-account-id"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "codex_cli_rs", r.Header.Get("originator"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, `{"credit_id":"credit-1","redeem_request_id":"redeem-1"}`, string(body))
		return `{"code":"reset"}`
	})

	statusCode, body, err := ConsumeCodexWhamRateLimitResetCredit(context.Background(), client, "https://upstream.example", "access-token", "account-123", "credit-1", "redeem-1")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, statusCode)
	require.JSONEq(t, `{"code":"reset"}`, string(body))
}

func TestResetCodexWhamUsageForwardsAutomaticResetJSONBody(t *testing.T) {
	client := codexWhamTestClient(t, func(r *http.Request) string {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/backend-api/wham/rate-limit-reset-credits/consume", r.URL.Path)
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		require.Equal(t, "account-123", r.Header.Get("chatgpt-account-id"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "codex_cli_rs", r.Header.Get("originator"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, `{"redeem_request_id":"redeem-auto"}`, string(body))
		return `{"code":"reset"}`
	})

	statusCode, body, err := ResetCodexWhamUsage(context.Background(), client, "https://upstream.example", "access-token", "account-123", "", "redeem-auto")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, statusCode)
	require.JSONEq(t, `{"code":"reset"}`, string(body))
}

func TestResetCodexWhamUsageForwardsSelectedCreditJSONBody(t *testing.T) {
	client := codexWhamTestClient(t, func(r *http.Request) string {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/backend-api/wham/rate-limit-reset-credits/consume", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, `{"credit_id":"credit-1","redeem_request_id":"redeem-1"}`, string(body))
		return `{"code":"reset"}`
	})

	statusCode, body, err := ResetCodexWhamUsage(context.Background(), client, "https://upstream.example", "access-token", "account-123", " credit-1 ", " redeem-1 ")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, statusCode)
	require.JSONEq(t, `{"code":"reset"}`, string(body))
}

func TestConsumeCodexWhamRateLimitResetCreditRejectsEmptyCreditID(t *testing.T) {
	var called bool
	client := codexWhamTestClient(t, func(r *http.Request) string {
		called = true
		return `{}`
	})

	statusCode, body, err := ConsumeCodexWhamRateLimitResetCredit(context.Background(), client, "https://upstream.example", "access-token", "account-123", " ", "redeem-1")
	require.Error(t, err)
	require.Equal(t, 0, statusCode)
	require.Nil(t, body)
	require.False(t, called)
}
