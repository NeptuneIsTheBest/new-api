package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

func FetchCodexWhamUsage(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error) {
	return doCodexWhamRequest(ctx, client, http.MethodGet, baseURL, "/backend-api/wham/usage", accessToken, accountID, nil)
}

func FetchCodexWhamRateLimitResetCredits(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error) {
	return doCodexWhamRequest(ctx, client, http.MethodGet, baseURL, "/backend-api/wham/rate-limit-reset-credits", accessToken, accountID, nil)
}

func ConsumeCodexWhamRateLimitResetCredit(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
	creditID string,
	redeemRequestID string,
) (statusCode int, body []byte, err error) {
	return consumeCodexWhamRateLimitResetCredit(ctx, client, baseURL, accessToken, accountID, creditID, redeemRequestID, true)
}

func ResetCodexWhamUsage(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
	creditID string,
	redeemRequestID string,
) (statusCode int, body []byte, err error) {
	return consumeCodexWhamRateLimitResetCredit(ctx, client, baseURL, accessToken, accountID, creditID, redeemRequestID, false)
}

func consumeCodexWhamRateLimitResetCredit(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
	creditID string,
	redeemRequestID string,
	requireCreditID bool,
) (statusCode int, body []byte, err error) {
	cid := strings.TrimSpace(creditID)
	if requireCreditID && cid == "" {
		return 0, nil, fmt.Errorf("empty creditID")
	}
	rrid := strings.TrimSpace(redeemRequestID)
	if rrid == "" {
		return 0, nil, fmt.Errorf("empty redeemRequestID")
	}

	payloadData := struct {
		CreditID        string `json:"credit_id,omitempty"`
		RedeemRequestID string `json:"redeem_request_id"`
	}{
		CreditID:        cid,
		RedeemRequestID: rrid,
	}

	payload, err := common.Marshal(payloadData)
	if err != nil {
		return 0, nil, err
	}

	return doCodexWhamRequest(ctx, client, http.MethodPost, baseURL, "/backend-api/wham/rate-limit-reset-credits/consume", accessToken, accountID, payload)
}

func doCodexWhamRequest(
	ctx context.Context,
	client *http.Client,
	method string,
	baseURL string,
	path string,
	accessToken string,
	accountID string,
	body []byte,
) (statusCode int, respBody []byte, err error) {
	if client == nil {
		return 0, nil, fmt.Errorf("nil http client")
	}
	bu := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if bu == "" {
		return 0, nil, fmt.Errorf("empty baseURL")
	}
	at := strings.TrimSpace(accessToken)
	aid := strings.TrimSpace(accountID)
	if at == "" {
		return 0, nil, fmt.Errorf("empty accessToken")
	}
	if aid == "" {
		return 0, nil, fmt.Errorf("empty accountID")
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, bu+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+at)
	req.Header.Set("chatgpt-account-id", aid)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("originator") == "" {
		req.Header.Set("originator", "codex_cli_rs")
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBody, nil
}
