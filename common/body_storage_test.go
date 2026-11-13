package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyStorageReader checks that the buffered prefix is already on disk before
// the storage consumes any more input. Checking only IsDisk after completion
// would also pass if the entire request had first been buffered in memory.
type bodyStorageReader struct {
	t                *testing.T
	reader           io.Reader
	readBytes        int64
	spillAt          int64
	fallbackAt       int64
	initialDiskFiles int
	initialDiskBytes int64
	checkedDisk      bool
	checkedFallback  bool
}

func (r *bodyStorageReader) Read(p []byte) (int, error) {
	r.t.Helper()
	if r.spillAt >= 0 && !r.checkedDisk {
		if r.readBytes < r.spillAt {
			p = p[:min(int64(len(p)), r.spillAt-r.readBytes)]
		} else {
			count, size, err := GetDiskCacheInfo()
			require.NoError(r.t, err)
			require.Equal(r.t, r.initialDiskFiles+1, count, "the body must spill before reading beyond the threshold")
			assert.Equal(r.t, r.initialDiskBytes+r.readBytes, size, "the prefix must be written before consuming more input")
			r.checkedDisk = true
		}
	}
	if r.fallbackAt > 0 && !r.checkedFallback {
		if r.readBytes < r.fallbackAt {
			p = p[:min(int64(len(p)), r.fallbackAt-r.readBytes)]
		} else {
			count, size, err := GetDiskCacheInfo()
			require.NoError(r.t, err)
			assert.Equal(r.t, r.initialDiskFiles, count, "fallback must remove the partial file before reading more input")
			assert.Equal(r.t, r.initialDiskBytes, size)
			r.checkedFallback = true
		}
	}
	n, err := r.reader.Read(p)
	r.readBytes += int64(n)
	return n, err
}

func TestCreateBodyStorageFromReader(t *testing.T) {
	previous := GetDiskCacheConfig()
	t.Cleanup(func() { SetDiskCacheConfig(previous) })

	const threshold = 1 << 20
	payload := bytes.Repeat([]byte("streamed body\n"), 2*threshold/13+1)
	tests := []struct {
		name          string
		size          int
		contentLength int64
		thresholdMB   int
		cacheMB       int
		occupiedMB    int
		maxBytes      int64
		disabled      bool
		wantDisk      bool
		spillAt       int64
		fallbackAt    int64
		dataWithEOF   bool
	}{
		{name: "unknown below threshold", size: threshold - 1, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, spillAt: -1},
		{name: "unknown at threshold", size: threshold, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: threshold},
		{name: "unknown above threshold", size: threshold + 1, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: threshold},
		{name: "actual size fits below request limit", size: 3 * threshold / 2, contentLength: -1, thresholdMB: 1, cacheMB: 2, maxBytes: 4 * threshold, wantDisk: true, spillAt: threshold},
		{name: "zero length hint", size: threshold + 1, contentLength: 0, thresholdMB: 1, cacheMB: 2, maxBytes: 4 * threshold, wantDisk: true, spillAt: threshold},
		{name: "underestimated length", size: threshold + 1, contentLength: 1, thresholdMB: 1, cacheMB: 2, maxBytes: 4 * threshold, wantDisk: true, spillAt: threshold},
		{name: "known large body streams immediately", size: threshold + 1, contentLength: threshold + 1, thresholdMB: 1, cacheMB: 2, maxBytes: 3 * threshold, wantDisk: true, spillAt: 0},
		{name: "known length outgrows cache", size: 3 * threshold / 2, contentLength: threshold, thresholdMB: 1, cacheMB: 1, maxBytes: 4 * threshold, spillAt: 0, fallbackAt: threshold + 1},
		{name: "empty body", contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, spillAt: -1},
		{name: "zero threshold", size: 17, contentLength: -1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: 0},
		{name: "zero threshold empty body", contentLength: -1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: 0},
		{name: "zero threshold without capacity", size: 17, contentLength: -1, maxBytes: 2 * threshold, spillAt: 0, fallbackAt: 1},
		{name: "cache disabled", size: threshold + 1, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, disabled: true, spillAt: -1},
		{name: "cache initially full", size: threshold + 1, contentLength: -1, thresholdMB: 1, cacheMB: 1, occupiedMB: 1, maxBytes: 2 * threshold, spillAt: -1},
		{name: "cache capacity exhausted", size: threshold + 1, contentLength: -1, thresholdMB: 1, cacheMB: 1, maxBytes: 2 * threshold, spillAt: threshold, fallbackAt: threshold + 1},
		{name: "capacity exhausted before request end", size: 3 * threshold / 2, contentLength: -1, thresholdMB: 1, cacheMB: 1, maxBytes: 4 * threshold, spillAt: threshold, fallbackAt: threshold + 1},
		{name: "capacity exhausted with final data and EOF", size: threshold + 17, contentLength: -1, thresholdMB: 1, cacheMB: 1, maxBytes: 4 * threshold, spillAt: threshold, dataWithEOF: true},
		{name: "threshold exactly fills cache", size: threshold, contentLength: -1, thresholdMB: 1, cacheMB: 1, maxBytes: 2 * threshold, wantDisk: true, spillAt: threshold},
		{name: "stream exactly fills cache", size: 2 * threshold, contentLength: -1, thresholdMB: 1, cacheMB: 2, maxBytes: 4 * threshold, wantDisk: true, spillAt: threshold},
		{name: "existing cache leaves enough capacity", size: 3 * threshold / 2, contentLength: -1, thresholdMB: 1, cacheMB: 3, occupiedMB: 1, maxBytes: 4 * threshold, wantDisk: true, spillAt: threshold},
		{name: "existing cache forces memory fallback", size: 3 * threshold / 2, contentLength: -1, thresholdMB: 1, cacheMB: 2, occupiedMB: 1, maxBytes: 4 * threshold, spillAt: threshold, fallbackAt: threshold + 1},
		{name: "request exactly at limit", size: 2 * threshold, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: threshold},
		{name: "request exactly at limit after fallback", size: 2 * threshold, contentLength: -1, thresholdMB: 1, cacheMB: 1, maxBytes: 2 * threshold, spillAt: threshold, fallbackAt: threshold + 1},
		{name: "limit below threshold", size: 17, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 17, spillAt: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			SetDiskCacheConfig(DiskCacheConfig{
				Enabled:     !test.disabled,
				ThresholdMB: test.thresholdMB,
				MaxSizeMB:   test.cacheMB,
				Path:        t.TempDir(),
			})
			if test.occupiedMB > 0 {
				occupied, err := CreateBodyStorage(payload[:test.occupiedMB<<20])
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, occupied.Close()) })
				require.True(t, occupied.IsDisk())
			}
			initialDiskFiles, initialDiskBytes, err := GetDiskCacheInfo()
			require.NoError(t, err)
			before := GetDiskCacheStats()
			data := payload[:test.size]
			var input io.Reader = bytes.NewReader(data)
			if test.dataWithEOF {
				input = iotest.DataErrReader(input)
			}
			reader := &bodyStorageReader{
				t: t, reader: input, spillAt: test.spillAt, fallbackAt: test.fallbackAt,
				initialDiskFiles: initialDiskFiles, initialDiskBytes: initialDiskBytes,
			}
			storage, err := CreateBodyStorageFromReader(reader, test.contentLength, test.maxBytes)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, storage.Close()) })
			assert.Equal(t, test.wantDisk, storage.IsDisk())
			assert.EqualValues(t, test.size, storage.Size())
			assert.Equal(t, test.spillAt >= 0, reader.checkedDisk)
			assert.Equal(t, test.fallbackAt > 0, reader.checkedFallback)

			wantStats := before
			if test.wantDisk {
				wantStats.ActiveDiskFiles++
				wantStats.CurrentDiskUsageBytes += int64(test.size)
				wantStats.DiskCacheHits++
			} else {
				wantStats.ActiveMemoryBuffers++
				wantStats.CurrentMemoryUsageBytes += int64(test.size)
				wantStats.MemoryCacheHits++
			}
			assert.Equal(t, wantStats, GetDiskCacheStats())

			first, err := io.ReadAll(storage)
			require.NoError(t, err)
			assert.Equal(t, data, first, "the initial cursor must be at the start")
			_, err = storage.Seek(0, io.SeekStart)
			require.NoError(t, err)
			replay, err := storage.NewReader()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, replay.Close()) })
			_, err = storage.Seek(storage.Size(), io.SeekStart)
			require.NoError(t, err)
			replayed, err := io.ReadAll(replay)
			require.NoError(t, err)
			assert.Equal(t, data, replayed, "replay must have its own cursor")
			require.NoError(t, storage.Close())
			require.NoError(t, storage.Close())
			_, err = storage.NewReader()
			require.ErrorIs(t, err, ErrStorageClosed)

			wantStats.ActiveDiskFiles = before.ActiveDiskFiles
			wantStats.CurrentDiskUsageBytes = before.CurrentDiskUsageBytes
			wantStats.ActiveMemoryBuffers = before.ActiveMemoryBuffers
			wantStats.CurrentMemoryUsageBytes = before.CurrentMemoryUsageBytes
			assert.Equal(t, wantStats, GetDiskCacheStats())
			count, size, err := GetDiskCacheInfo()
			require.NoError(t, err)
			assert.Equal(t, initialDiskFiles, count)
			assert.Equal(t, initialDiskBytes, size)
		})
	}
}

func TestCreateBodyStorageFromReaderErrors(t *testing.T) {
	previous := GetDiskCacheConfig()
	t.Cleanup(func() { SetDiskCacheConfig(previous) })

	const threshold = 1 << 20
	payload := bytes.Repeat([]byte("streamed body\n"), 2*threshold/13+1)
	readErr := errors.New("body read failed")
	tests := []struct {
		name          string
		size          int
		maxBytes      int64
		cacheMB       int
		spillAt       int64
		fallbackAt    int64
		readError     error
		wantError     error
		invalidPath   bool
		httpLimit     bool
		wantReadBytes int64
	}{
		{name: "oversized before threshold", size: 18, maxBytes: 17, cacheMB: 4, spillAt: -1, wantError: ErrRequestBodyTooLarge, wantReadBytes: 18},
		{name: "oversized after spill", size: 2*threshold + 1, maxBytes: 2 * threshold, cacheMB: 4, spillAt: threshold, wantError: ErrRequestBodyTooLarge, wantReadBytes: 2*threshold + 1},
		{name: "read error before threshold", size: 17, maxBytes: 2 * threshold, cacheMB: 4, spillAt: -1, readError: readErr, wantError: readErr, wantReadBytes: 17},
		{name: "read error after spill", size: threshold + 17, maxBytes: 2 * threshold, cacheMB: 4, spillAt: threshold, readError: readErr, wantError: readErr, wantReadBytes: threshold + 17},
		{name: "HTTP body limit after spill", size: 2*threshold + 1, maxBytes: 2 * threshold, cacheMB: 4, spillAt: threshold, httpLimit: true, wantReadBytes: 2 * threshold},
		{name: "oversized after fallback", size: 2*threshold + 1, maxBytes: 2 * threshold, cacheMB: 1, spillAt: threshold, fallbackAt: threshold + 1, wantError: ErrRequestBodyTooLarge, wantReadBytes: 2*threshold + 1},
		{name: "read error after fallback", size: threshold + 17, maxBytes: 2 * threshold, cacheMB: 1, spillAt: threshold, fallbackAt: threshold + 1, readError: readErr, wantError: readErr, wantReadBytes: threshold + 17},
		{name: "HTTP body limit after fallback", size: 2*threshold + 1, maxBytes: 2 * threshold, cacheMB: 1, spillAt: threshold, fallbackAt: threshold + 1, httpLimit: true, wantReadBytes: 2 * threshold},
		{name: "cache creation failure", size: threshold + 17, maxBytes: 2 * threshold, cacheMB: 4, spillAt: -1, invalidPath: true, wantReadBytes: threshold},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := t.TempDir()
			if test.invalidPath {
				path = filepath.Join(path, "not-a-directory")
				require.NoError(t, os.WriteFile(path, []byte("blocked"), 0600))
			}
			SetDiskCacheConfig(DiskCacheConfig{Enabled: true, ThresholdMB: 1, MaxSizeMB: test.cacheMB, Path: path})
			before := GetDiskCacheStats()
			var input io.Reader = bytes.NewReader(payload[:test.size])
			if test.readError != nil {
				input = io.MultiReader(input, iotest.ErrReader(test.readError))
			}
			if test.httpLimit {
				body := http.MaxBytesReader(nil, io.NopCloser(input), test.maxBytes)
				t.Cleanup(func() { require.NoError(t, body.Close()) })
				input = body
			}
			reader := &bodyStorageReader{t: t, reader: input, spillAt: test.spillAt, fallbackAt: test.fallbackAt}
			storage, err := CreateBodyStorageFromReader(reader, -1, test.maxBytes)
			if storage != nil {
				t.Cleanup(func() { require.NoError(t, storage.Close()) })
			}
			require.Error(t, err)
			assert.Nil(t, storage)
			if test.wantError != nil {
				require.ErrorIs(t, err, test.wantError)
			}
			if test.httpLimit {
				var limitErr *http.MaxBytesError
				require.ErrorAs(t, err, &limitErr)
			}
			assert.Equal(t, test.wantError == ErrRequestBodyTooLarge || test.httpLimit, IsRequestBodyTooLargeError(err))
			assert.Equal(t, test.wantReadBytes, reader.readBytes)
			assert.Equal(t, test.spillAt >= 0, reader.checkedDisk)
			assert.Equal(t, test.fallbackAt > 0, reader.checkedFallback)
			assert.Equal(t, before, GetDiskCacheStats(), "failed reads must not add storage or cache hits")
			if test.invalidPath {
				entries, err := os.ReadDir(filepath.Dir(path))
				require.NoError(t, err)
				assert.Len(t, entries, 1)
			} else {
				count, size, err := GetDiskCacheInfo()
				require.NoError(t, err)
				assert.Zero(t, count, "failed reads must remove partial cache files")
				assert.Zero(t, size)
			}
		})
	}
}

func TestReplayableBodiesOverOneStorageKeepOwnCursors(t *testing.T) {
	previous := GetDiskCacheConfig()
	t.Cleanup(func() { SetDiskCacheConfig(previous) })
	payload := []byte(`{"model":"test-model","input":"` + strings.Repeat("x", 4<<10) + `"}`)
	for _, disk := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "file"}[disk], func(t *testing.T) {
			SetDiskCacheConfig(DiskCacheConfig{Enabled: disk, ThresholdMB: 0, MaxSizeMB: 16, Path: t.TempDir()})
			storage, err := CreateBodyStorage(payload)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, storage.Close()) })
			require.Equal(t, disk, storage.IsDisk())

			// The transport may still be writing attempt 1 when the retry loop
			// seeks the shared cursor and attempt 2 forwards the same body.
			first, err := http.NewRequest(http.MethodPost, "https://example.com", NewReplayableBodyReader(storage))
			require.NoError(t, err)
			head := make([]byte, len(payload)/2)
			_, err = io.ReadFull(first.Body, head)
			require.NoError(t, err)
			_, err = storage.Seek(0, io.SeekStart)
			require.NoError(t, err)
			secondBody := NewReplayableBodyReader(storage)
			second, err := http.NewRequest(http.MethodPost, "https://example.com", secondBody)
			require.NoError(t, err)
			sent, err := io.ReadAll(second.Body)
			require.NoError(t, err)
			assert.Equal(t, payload, sent)
			rest, err := io.ReadAll(first.Body)
			require.NoError(t, err)
			assert.Equal(t, payload, append(head, rest...))

			// Closing request bodies, as net/http does, keeps the storage for
			// GetBody replays and later attempts.
			require.NoError(t, first.Body.Close())
			require.NoError(t, second.Body.Close())
			_, err = first.Body.Read(make([]byte, 1))
			assert.ErrorIs(t, err, http.ErrBodyReadAfterClose)
			replay, err := secondBody.NewReader()
			require.NoError(t, err)
			replayed, err := io.ReadAll(replay)
			require.NoError(t, err)
			require.NoError(t, replay.Close())
			assert.Equal(t, payload, replayed)

			require.NoError(t, storage.Close())
			_, err = io.ReadAll(NewReplayableBodyReader(storage))
			assert.ErrorIs(t, err, ErrStorageClosed)
		})
	}
}

func TestUnmarshalBodyReusableNoCopySurvivesBodyReplacement(t *testing.T) {
	previous := GetDiskCacheConfig()
	t.Cleanup(func() { SetDiskCacheConfig(previous) })
	input := strings.Repeat("image-data", 32<<10)
	payload := []byte(`{"model":"first","input":"` + input + `","metadata":{"count":0,"enabled":false}}`)
	for _, disk := range []bool{false, true} {
		name := "memory"
		if disk {
			name = "file"
		}
		t.Run(name, func(t *testing.T) {
			SetDiskCacheConfig(DiskCacheConfig{Enabled: disk, ThresholdMB: 0, MaxSizeMB: 16, Path: t.TempDir()})
			storage, err := CreateBodyStorage(payload)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, storage.Close()) })
			require.Equal(t, disk, storage.IsDisk())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(KeyBodyStorage, storage)
			var decoded struct {
				Model    string          `json:"model"`
				Input    string          `json:"input"`
				Metadata json.RawMessage `json:"metadata"`
			}
			require.NoError(t, UnmarshalBodyReusableNoCopy(c, &decoded))
			assert.Equal(t, "first", decoded.Model)
			assert.Equal(t, input, decoded.Input)
			assert.Equal(t, `{"count":0,"enabled":false}`, string(decoded.Metadata))
			replayed, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			assert.Equal(t, payload, replayed)
			replacement, err := CreateBodyStorage([]byte(`{"model":"second"}`))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, replacement.Close()) })
			c.Set(KeyBodyStorage, replacement)
			require.NoError(t, storage.Close())
			var next map[string]any
			require.NoError(t, UnmarshalBodyReusableNoCopy(c, &next))
			assert.Equal(t, "second", next["model"])
			assert.Equal(t, "first", decoded.Model)
			assert.Equal(t, input, decoded.Input)
			assert.Equal(t, `{"count":0,"enabled":false}`, string(decoded.Metadata))
		})
	}
}

func TestMultipartBodyStorageReplaysFilesAndFields(t *testing.T) {
	previous := GetDiskCacheConfig()
	previousLimit := constant.MaxFileDownloadMB
	t.Cleanup(func() { SetDiskCacheConfig(previous); constant.MaxFileDownloadMB = previousLimit })
	constant.MaxFileDownloadMB = 1
	fileContents := bytes.Repeat([]byte("image"), 300000)
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	require.NoError(t, writer.WriteField("tag", "one"))
	require.NoError(t, writer.WriteField("tag", "two"))
	part, err := writer.CreateFormFile("image", "image.png")
	require.NoError(t, err)
	_, err = part.Write(fileContents)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	for _, disk := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "file"}[disk], func(t *testing.T) {
			SetDiskCacheConfig(DiskCacheConfig{Enabled: disk, ThresholdMB: 0, MaxSizeMB: 16, Path: t.TempDir()})
			storage, err := CreateBodyStorage(payload.Bytes())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, storage.Close()) })
			require.Equal(t, disk, storage.IsDisk())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(payload.Bytes()))
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			c.Set(KeyBodyStorage, storage)
			form, err := ParseMultipartFormReusable(c)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, form.RemoveAll()) })
			assert.Equal(t, []string{"one", "two"}, form.Value["tag"])
			require.Len(t, form.File["image"], 1)
			file, err := form.File["image"][0].Open()
			require.NoError(t, err)
			actual, err := io.ReadAll(file)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			assert.Equal(t, fileContents, actual)
			// Upstream rebuilding may have replaced the multipart boundary.
			c.Request.Header.Set("Content-Type", "multipart/form-data; boundary=outbound")
			var fields map[string]any
			require.NoError(t, UnmarshalBodyReusableNoCopy(c, &fields))
			assert.Equal(t, map[string]any{"tag": []any{"one", "two"}}, fields)
			replay, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			assert.Equal(t, payload.Bytes(), replay)
		})
	}
}

func TestReusableJSONBodyRequiresOneCompleteValue(t *testing.T) {
	previous := GetDiskCacheConfig()
	t.Cleanup(func() { SetDiskCacheConfig(previous) })
	for _, contentType := range []string{"application/json", "multipart/form-data"} {
		for _, disk := range []bool{false, true} {
			for _, suffix := range []string{" \n\t", " {}", " null", " garbage"} {
				t.Run(contentType+"/"+map[bool]string{false: "memory", true: "file"}[disk]+"/"+suffix, func(t *testing.T) {
					SetDiskCacheConfig(DiskCacheConfig{Enabled: disk, ThresholdMB: 0, MaxSizeMB: 16, Path: t.TempDir()})
					body := `{"ignored":"` + strings.Repeat("a", 256<<10) + `","n":0}` + suffix
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/body", strings.NewReader(body))
					c.Request.Header.Set("Content-Type", contentType)
					t.Cleanup(func() { CleanupBodyStorage(c) })
					var decoded struct {
						N *uint `json:"n"`
					}
					err := UnmarshalBodyReusableNoCopy(c, &decoded)
					if strings.TrimSpace(suffix) != "" {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
					require.NotNil(t, decoded.N)
					assert.Zero(t, *decoded.N)
				})
			}
		}
	}
}

func BenchmarkUnmarshalBodyReusableImageRequest(b *testing.B) {
	previous := GetDiskCacheConfig()
	SetDiskCacheConfig(DiskCacheConfig{Path: b.TempDir()})
	b.Cleanup(func() { SetDiskCacheConfig(previous) })
	payload := []byte(`{"model":"image-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", 1<<20) + `"}}]}]}`)
	for _, mode := range []string{"memory_copy", "memory_no_copy", "file_read_all", "file_stream"} {
		b.Run(mode, func(b *testing.B) {
			var storage BodyStorage
			if strings.HasPrefix(mode, "file") {
				var err error
				storage, err = newDiskStorage(payload, GetDiskCachePath())
				require.NoError(b, err)
			} else {
				storage = newMemoryStorage(payload)
			}
			b.Cleanup(func() { require.NoError(b, storage.Close()) })
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(KeyBodyStorage, storage)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for b.Loop() {
				var request dto.GeneralOpenAIRequest
				var err error
				switch mode {
				case "memory_no_copy":
					err = UnmarshalBodyReusableNoCopy(c, &request)
				case "file_read_all":
					var data []byte
					data, err = storage.Bytes()
					if err == nil {
						err = Unmarshal(data, &request)
					}
				default:
					err = UnmarshalBodyReusable(c, &request)
				}
				require.NoError(b, err)
			}
		})
	}
}

func BenchmarkMultipartBodyStorage(b *testing.B) {
	previous := GetDiskCacheConfig()
	previousLimit := constant.MaxFileDownloadMB
	SetDiskCacheConfig(DiskCacheConfig{Path: b.TempDir()})
	constant.MaxFileDownloadMB = 1
	b.Cleanup(func() { SetDiskCacheConfig(previous); constant.MaxFileDownloadMB = previousLimit })
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	require.NoError(b, writer.WriteField("model", "image-model"))
	part, err := writer.CreateFormFile("image", "image.png")
	require.NoError(b, err)
	_, err = io.Copy(part, strings.NewReader(strings.Repeat("A", 8<<20)))
	require.NoError(b, err)
	require.NoError(b, writer.Close())
	storage, err := newDiskStorage(payload.Bytes(), GetDiskCachePath())
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, storage.Close()) })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	c.Set(KeyBodyStorage, storage)
	for _, mode := range []string{"file_read_all", "file_stream"} {
		b.Run(mode, func(b *testing.B) {
			b.SetBytes(storage.Size())
			b.ReportAllocs()
			for b.Loop() {
				var form *multipart.Form
				if mode == "file_read_all" {
					body, err := storage.Bytes()
					require.NoError(b, err)
					form, err = multipart.NewReader(bytes.NewReader(body), writer.Boundary()).ReadForm(multipartMemoryLimit())
					require.NoError(b, err)
				} else {
					var err error
					form, err = ParseMultipartFormReusable(c)
					require.NoError(b, err)
				}
				require.NoError(b, form.RemoveAll())
			}
		})
	}
}
