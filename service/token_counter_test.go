package service

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCountRequestTokenMediaLoading(t *testing.T) {
	previousMedia, previousNonStream := constant.GetMediaToken, constant.GetMediaTokenNotStream
	previousMaxSize := constant.MaxFileDownloadMB
	previousClient, previousWorker := httpClient, system_setting.WorkerUrl
	fetch := system_setting.GetFetchSetting()
	previousFetch := *fetch
	previousDiskCache := common.GetDiskCacheConfig()
	t.Cleanup(func() {
		constant.GetMediaToken, constant.GetMediaTokenNotStream = previousMedia, previousNonStream
		constant.MaxFileDownloadMB = previousMaxSize
		httpClient, system_setting.WorkerUrl = previousClient, previousWorker
		*fetch = previousFetch
		common.SetDiskCacheConfig(previousDiskCache)
	})
	constant.MaxFileDownloadMB = 1
	system_setting.WorkerUrl = ""
	fetch.EnableSSRFProtection = false
	common.SetDiskCacheConfig(common.DiskCacheConfig{})

	var imageData bytes.Buffer
	require.NoError(t, png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 200, 200))))

	for _, tc := range []struct {
		name             string
		model            string
		detail           string
		fileType         types.FileType
		mimeType         string
		disableMedia     bool
		disableNonStream bool
		stream           bool
		copies           int
		downloadFails    bool
		wantDownloads    int32
		wantTokens       int
		wantError        bool
	}{
		{
			name: "tile low detail skips unavailable image", model: "gpt-4o", detail: "low",
			fileType: types.FileTypeImage, downloadFails: true, stream: true,
			wantTokens: 85 + 3,
		},
		{
			name: "Gemini resolution skips unavailable image", model: "gemini-3.7-flash", detail: "MEDIA_RESOLUTION_LOW",
			fileType: types.FileTypeImage, downloadFails: true,
			wantTokens: 280 + 3,
		},
		{
			name: "unknown model skips unavailable image", model: "deepseek-v4",
			fileType: types.FileTypeImage, downloadFails: true,
			wantTokens: 520 + 3,
		},
		{
			name: "high detail measures and reuses URL cache", model: "gpt-4o", detail: "high",
			fileType: types.FileTypeImage, copies: 2,
			wantDownloads: 1, wantTokens: 2*(85+170) + 3,
		},
		{
			name: "patch low detail still needs dimensions", model: "gpt-5.6-sol", detail: "low",
			fileType:      types.FileTypeImage,
			wantDownloads: 1, wantTokens: 59 + 3,
		},
		{
			name: "required image download failure", model: "gpt-4o", detail: "high",
			fileType: types.FileTypeImage, downloadFails: true,
			wantDownloads: 1, wantError: true,
		},
		{
			name: "disabled media measurement assumes default size", model: "gpt-4o", detail: "high",
			fileType: types.FileTypeImage, disableMedia: true, stream: true, downloadFails: true,
			wantTokens: 85 + 170*4 + 3,
		},
		{
			name: "disabled non-stream measurement assumes default size", model: "gpt-4o", detail: "high",
			fileType: types.FileTypeImage, disableNonStream: true, downloadFails: true,
			wantTokens: 85 + 170*4 + 3,
		},
		{
			name: "non-stream switch still allows stream measurement", model: "gpt-4o", detail: "high",
			fileType: types.FileTypeImage, disableNonStream: true, stream: true,
			wantDownloads: 1, wantTokens: 85 + 170 + 3,
		},
		{
			name: "unknown file type still needs detection", model: "gpt-4o", detail: "low",
			wantDownloads: 1, wantTokens: 85 + 3,
		},
		{
			name: "generic file can still be detected as image", model: "gpt-4o", detail: "low",
			fileType:      types.FileTypeFile,
			wantDownloads: 1, wantTokens: 85 + 3,
		},
		{
			name: "audio keeps existing loading and estimate", model: "gpt-4o", detail: "low",
			fileType: types.FileTypeAudio, mimeType: "audio/mpeg",
			wantDownloads: 1, wantTokens: 256 + 3,
		},
		{
			name: "video keeps existing loading and estimate", model: "gpt-4o", detail: "low",
			fileType: types.FileTypeVideo, mimeType: "video/mp4",
			wantDownloads: 1, wantTokens: 4096*2 + 3,
		},
		{
			name: "document keeps existing loading and estimate", model: "gpt-4o", detail: "low",
			fileType: types.FileTypeFile, mimeType: "application/pdf",
			wantDownloads: 1, wantTokens: 4096 + 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			constant.GetMediaToken = !tc.disableMedia
			constant.GetMediaTokenNotStream = !tc.disableNonStream
			var downloads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads.Add(1)
				if tc.downloadFails {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if tc.mimeType == "" {
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write(imageData.Bytes())
					return
				}
				w.Header().Set("Content-Type", tc.mimeType)
				_, _ = w.Write([]byte("media content"))
			}))
			t.Cleanup(server.Close)
			httpClient = server.Client()

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			common.SetContextKey(c, constant.ContextKeyOriginalModel, tc.model)
			t.Cleanup(func() { CleanupFileSources(c) })
			meta := &types.TokenCountMeta{}
			for range max(1, tc.copies) {
				meta.Files = append(meta.Files, &types.FileMeta{
					FileType: tc.fileType,
					Source:   types.NewURLFileSource(server.URL),
					Detail:   tc.detail,
				})
			}
			// OpenAI format enables the preload path even for Gemini models.
			tokens, err := CountRequestToken(c, meta, &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatOpenAI,
				IsStream:    tc.stream,
			})

			assert.Equal(t, tc.wantDownloads, downloads.Load())
			if tc.wantError {
				require.ErrorContains(t, err, "status code: 503")
				assert.Zero(t, tokens)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantTokens, tokens)
		})
	}
}
