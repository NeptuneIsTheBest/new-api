package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesHelperAllowsAdvancedCustomResponsesCompact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeAdvancedCustom)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "sk-test")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4o-mini")
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
		AdvancedCustom: &dto.AdvancedCustomConfig{
			Routes: []dto.AdvancedCustomRoute{
				{
					IncomingPath: "/v1/responses/compact",
					UpstreamPath: "/v1/responses/compact",
					Converter:    dto.AdvancedCustomConverterNone,
				},
			},
		},
	})

	info := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeResponsesCompact,
		RelayFormat:     types.RelayFormatOpenAIResponsesCompaction,
		OriginModelName: "gpt-4o-mini",
		RequestURLPath:  "/v1/responses/compact",
		Request: &dto.OpenAIResponsesCompactionRequest{
			Model: "gpt-4o-mini",
			Input: []byte(`"test"`),
		},
	}

	newAPIError := ResponsesHelper(c, info)

	require.NotNil(t, newAPIError)
	assert.Equal(t, types.ErrorCodeDoRequestFailed, newAPIError.GetErrorCode())
	assert.Contains(t, newAPIError.Error(), "channel base URL is required")
	assert.NotContains(t, newAPIError.Error(), "unsupported endpoint")
}
