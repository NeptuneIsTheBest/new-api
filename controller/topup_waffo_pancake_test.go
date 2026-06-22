package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatWaffoPancakeAmount_UsesDisplayPriceString(t *testing.T) {
	testCases := []struct {
		name     string
		amount   float64
		expected string
	}{
		{name: "whole amount", amount: 29, expected: "29.00"},
		{name: "decimal amount", amount: 29.9, expected: "29.90"},
		{name: "round half up to cents", amount: 29.999, expected: "30.00"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, formatWaffoPancakeAmount(tc.amount))
		})
	}
}

func TestGetWaffoPancakePayMoney(t *testing.T) {
	originalUnitPrice := setting.WaffoPancakeUnitPrice
	originalQuotaDisplayType := operation_setting.GetGeneralSetting().QuotaDisplayType
	originalDiscounts := make(map[int]float64, len(operation_setting.GetPaymentSetting().AmountDiscount))
	for k, v := range operation_setting.GetPaymentSetting().AmountDiscount {
		originalDiscounts[k] = v
	}
	originalTopupGroupRatio := common.TopupGroupRatio2JSONString()

	t.Cleanup(func() {
		setting.WaffoPancakeUnitPrice = originalUnitPrice
		operation_setting.GetGeneralSetting().QuotaDisplayType = originalQuotaDisplayType
		operation_setting.GetPaymentSetting().AmountDiscount = originalDiscounts
		require.NoError(t, common.UpdateTopupGroupRatioByJSONString(originalTopupGroupRatio))
	})

	setting.WaffoPancakeUnitPrice = 2.5
	operation_setting.GetPaymentSetting().AmountDiscount = map[int]float64{
		10:                           0.8,
		int(common.QuotaPerUnit * 3): 0.5,
		20:                           0,
	}
	require.NoError(t, common.UpdateTopupGroupRatioByJSONString(`{"default":1,"vip":1.2}`))

	testCases := []struct {
		name             string
		amount           int64
		group            string
		quotaDisplayType string
		expected         float64
	}{
		{
			name:             "currency display applies unit price group ratio and discount",
			amount:           10,
			group:            "vip",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			expected:         24,
		},
		{
			name:             "tokens display converts quota to display units before pricing",
			amount:           int64(common.QuotaPerUnit * 3),
			group:            "vip",
			quotaDisplayType: operation_setting.QuotaDisplayTypeTokens,
			expected:         4.5,
		},
		{
			name:             "non-positive discount falls back to no discount",
			amount:           20,
			group:            "default",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			expected:         50,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			operation_setting.GetGeneralSetting().QuotaDisplayType = tc.quotaDisplayType
			actual := getWaffoPancakePayMoney(tc.amount, tc.group)
			require.InDelta(t, tc.expected, actual, 0.000001)
		})
	}
}

func TestListWaffoPancakeCatalog_GETIgnoresCredentialQuery(t *testing.T) {
	setWaffoPancakeCatalogCredentialsForTest(t, "persisted-merchant", "persisted-private")

	var gotMerchantID string
	var gotPrivateKey string
	var callCount int
	stubWaffoPancakeCatalogLister(t, func(ctx context.Context, merchantID, privateKey string) (*service.WaffoPancakeCatalog, error) {
		callCount++
		gotMerchantID = merchantID
		gotPrivateKey = privateKey
		return &service.WaffoPancakeCatalog{}, nil
	})

	recorder := performWaffoPancakeCatalogRequest(
		http.MethodGet,
		"/catalog?merchant_id=query-merchant&private_key=query-private",
		"",
	)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, 1, callCount)
	assert.Equal(t, "persisted-merchant", gotMerchantID)
	assert.Equal(t, "persisted-private", gotPrivateKey)
}

func TestListWaffoPancakeCatalog_POSTUsesBodyCredentials(t *testing.T) {
	setWaffoPancakeCatalogCredentialsForTest(t, "persisted-merchant", "persisted-private")

	var gotMerchantID string
	var gotPrivateKey string
	var callCount int
	stubWaffoPancakeCatalogLister(t, func(ctx context.Context, merchantID, privateKey string) (*service.WaffoPancakeCatalog, error) {
		callCount++
		gotMerchantID = merchantID
		gotPrivateKey = privateKey
		return &service.WaffoPancakeCatalog{}, nil
	})

	recorder := performWaffoPancakeCatalogRequest(
		http.MethodPost,
		"/catalog",
		`{"merchant_id":"body-merchant","private_key":"body-private"}`,
	)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, 1, callCount)
	assert.Equal(t, "body-merchant", gotMerchantID)
	assert.Equal(t, "body-private", gotPrivateKey)
}

func TestListWaffoPancakeCatalog_POSTBlankBodyFallsBackToPersistedCredentials(t *testing.T) {
	setWaffoPancakeCatalogCredentialsForTest(t, "persisted-merchant", "persisted-private")

	var gotMerchantID string
	var gotPrivateKey string
	var callCount int
	stubWaffoPancakeCatalogLister(t, func(ctx context.Context, merchantID, privateKey string) (*service.WaffoPancakeCatalog, error) {
		callCount++
		gotMerchantID = merchantID
		gotPrivateKey = privateKey
		return &service.WaffoPancakeCatalog{}, nil
	})

	recorder := performWaffoPancakeCatalogRequest(http.MethodPost, "/catalog", "")

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, 1, callCount)
	assert.Equal(t, "persisted-merchant", gotMerchantID)
	assert.Equal(t, "persisted-private", gotPrivateKey)
}

func setWaffoPancakeCatalogCredentialsForTest(t *testing.T, merchantID, privateKey string) {
	t.Helper()

	originalMerchantID := setting.WaffoPancakeMerchantID
	originalPrivateKey := setting.WaffoPancakePrivateKey
	setting.WaffoPancakeMerchantID = merchantID
	setting.WaffoPancakePrivateKey = privateKey

	t.Cleanup(func() {
		setting.WaffoPancakeMerchantID = originalMerchantID
		setting.WaffoPancakePrivateKey = originalPrivateKey
	})
}

func stubWaffoPancakeCatalogLister(t *testing.T, lister func(context.Context, string, string) (*service.WaffoPancakeCatalog, error)) {
	t.Helper()

	originalLister := waffoPancakeCatalogLister
	waffoPancakeCatalogLister = lister

	t.Cleanup(func() {
		waffoPancakeCatalogLister = originalLister
	})
}

func performWaffoPancakeCatalogRequest(method string, target string, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/catalog", ListWaffoPancakeCatalog)
	router.POST("/catalog", ListWaffoPancakeCatalog)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}
