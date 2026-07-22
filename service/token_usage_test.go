package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetTokenUsageDetailsReportsUnavailableWithoutQueryingLogs(t *testing.T) {
	originalLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() {
		common.LogConsumeEnabled = originalLogConsumeEnabled
	})

	token := &model.Token{
		Id:             1,
		UserId:         7,
		CreatedTime:    10,
		UsageResetTime: 20,
	}
	details, err := GetTokenUsageDetails(7, token, time.Unix(30, 0), "Asia/Shanghai")
	require.NoError(t, err)
	require.NotNil(t, details)
	assert.False(t, details.Available)
	assert.Equal(t, int64(20), details.RangeStart)
	assert.Empty(t, details.RangeStartNano)
	assert.True(t, details.RangeStartExclusive)
	assert.Equal(t, int64(30), details.RangeEnd)
	assert.Equal(t, "30000000000", details.RangeEndNano)
	assert.Equal(t, "Asia/Shanghai", details.Timezone)
	assert.Equal(t, "day", details.BucketUnit)
	assert.Empty(t, details.Trend)
	assert.Empty(t, details.Models)
}

func TestGetTokenUsageDetailsExposesExactWriteWindow(t *testing.T) {
	originalLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() {
		common.LogConsumeEnabled = originalLogConsumeEnabled
	})

	token := &model.Token{
		Id:                 1,
		UserId:             7,
		CreatedTime:        10,
		UsageResetTime:     20,
		UsageResetTimeNano: 20_500,
	}
	details, err := GetTokenUsageDetails(7, token, time.Unix(30, 123), "")
	require.NoError(t, err)
	require.NotNil(t, details)
	assert.Equal(t, "20500", details.RangeStartNano)
	assert.True(t, details.RangeStartExclusive)
	assert.Equal(t, "30000000123", details.RangeEndNano)
	assert.Equal(t, "UTC", details.Timezone)
}

func TestGetTokenUsageDetailsRejectsInvalidTimezone(t *testing.T) {
	token := &model.Token{Id: 1, UserId: 7, CreatedTime: 10}

	_, err := GetTokenUsageDetails(7, token, time.Unix(30, 0), "Mars/Olympus_Mons")
	require.Error(t, err)

	_, err = GetTokenUsageDetails(7, token, time.Unix(30, 0), "Local")
	require.Error(t, err)
}
