package common

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyStorageReader checks that the buffered prefix is already on disk before
// the storage consumes any more input. Checking only IsDisk after completion
// would also pass if the entire request had first been buffered in memory.
type bodyStorageReader struct {
	t           *testing.T
	reader      io.Reader
	readBytes   int64
	spillAt     int64
	checkedDisk bool
}

func (r *bodyStorageReader) Read(p []byte) (int, error) {
	r.t.Helper()
	if r.spillAt >= 0 && !r.checkedDisk {
		if r.readBytes < r.spillAt {
			p = p[:min(int64(len(p)), r.spillAt-r.readBytes)]
		} else {
			count, size, err := GetDiskCacheInfo()
			require.NoError(r.t, err)
			require.Equal(r.t, 1, count, "the body must spill before reading beyond the threshold")
			assert.Equal(r.t, r.readBytes, size, "the prefix must be written before consuming more input")
			r.checkedDisk = true
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
		maxBytes      int64
		disabled      bool
		wantDisk      bool
		spillAt       int64
	}{
		{name: "unknown below threshold", size: threshold - 1, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, spillAt: -1},
		{name: "unknown at threshold", size: threshold, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: threshold},
		{name: "unknown above threshold", size: threshold + 1, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: threshold},
		{name: "zero length hint", size: threshold + 1, contentLength: 0, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: threshold},
		{name: "underestimated length", size: threshold + 1, contentLength: 1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: threshold},
		{name: "known large body streams immediately", size: threshold + 1, contentLength: threshold + 1, thresholdMB: 1, cacheMB: 2, maxBytes: 3 * threshold, wantDisk: true, spillAt: 0},
		{name: "empty body", contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, spillAt: -1},
		{name: "zero threshold", size: 17, contentLength: -1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: 0},
		{name: "zero threshold empty body", contentLength: -1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: 0},
		{name: "cache disabled", size: threshold + 1, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, disabled: true, spillAt: -1},
		{name: "cache capacity insufficient", size: threshold + 1, contentLength: -1, thresholdMB: 1, cacheMB: 1, maxBytes: 2 * threshold, spillAt: -1},
		{name: "actual size fits cache after memory fallback", size: threshold, contentLength: -1, thresholdMB: 1, cacheMB: 1, maxBytes: 2 * threshold, wantDisk: true, spillAt: -1},
		{name: "request exactly at limit", size: 2 * threshold, contentLength: -1, thresholdMB: 1, cacheMB: 4, maxBytes: 2 * threshold, wantDisk: true, spillAt: threshold},
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
			before := GetDiskCacheStats()
			data := payload[:test.size]
			reader := &bodyStorageReader{t: t, reader: bytes.NewReader(data), spillAt: test.spillAt}
			storage, err := CreateBodyStorageFromReader(reader, test.contentLength, test.maxBytes)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, storage.Close()) })
			assert.Equal(t, test.wantDisk, storage.IsDisk())
			assert.EqualValues(t, test.size, storage.Size())
			assert.Equal(t, test.spillAt >= 0, reader.checkedDisk)

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
			assert.Zero(t, count)
			assert.Zero(t, size)
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
		spillAt       int64
		readError     error
		wantError     error
		invalidPath   bool
		httpLimit     bool
		wantReadBytes int64
	}{
		{name: "oversized before threshold", size: 18, maxBytes: 17, spillAt: -1, wantError: ErrRequestBodyTooLarge, wantReadBytes: 18},
		{name: "oversized after spill", size: 2*threshold + 1, maxBytes: 2 * threshold, spillAt: threshold, wantError: ErrRequestBodyTooLarge, wantReadBytes: 2*threshold + 1},
		{name: "read error before threshold", size: 17, maxBytes: 2 * threshold, spillAt: -1, readError: readErr, wantError: readErr, wantReadBytes: 17},
		{name: "read error after spill", size: threshold + 17, maxBytes: 2 * threshold, spillAt: threshold, readError: readErr, wantError: readErr, wantReadBytes: threshold + 17},
		{name: "HTTP body limit after spill", size: 2*threshold + 1, maxBytes: 2 * threshold, spillAt: threshold, httpLimit: true, wantReadBytes: 2 * threshold},
		{name: "cache creation failure", size: threshold + 17, maxBytes: 2 * threshold, spillAt: -1, invalidPath: true, wantReadBytes: threshold},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := t.TempDir()
			if test.invalidPath {
				path = filepath.Join(path, "not-a-directory")
				require.NoError(t, os.WriteFile(path, []byte("blocked"), 0600))
			}
			SetDiskCacheConfig(DiskCacheConfig{Enabled: true, ThresholdMB: 1, MaxSizeMB: 4, Path: path})
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
			reader := &bodyStorageReader{t: t, reader: input, spillAt: test.spillAt}
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

func TestNewReplayableBodyReaderKeepsStorageLifecycleWithCaller(t *testing.T) {
	payload := []byte(`{"model":"test-model","input":"hello"}`)
	storage, err := CreateBodyStorage(payload)
	require.NoError(t, err)
	defer storage.Close()

	body := NewReplayableBodyReader(storage)
	assert.EqualValues(t, len(payload), body.Size())
	_, exposesCloser := any(body).(io.Closer)
	assert.False(t, exposesCloser, "the request body must not expose the storage closer")

	req, err := http.NewRequest(http.MethodPost, "https://example.com", body)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())

	replayBody, err := body.NewReader()
	require.NoError(t, err, "closing the HTTP request body must not close the storage")
	replay, err := io.ReadAll(replayBody)
	require.NoError(t, err)
	require.NoError(t, replayBody.Close())
	assert.Equal(t, payload, replay)

	require.NoError(t, storage.Close())
	_, err = body.NewReader()
	require.ErrorIs(t, err, ErrStorageClosed)
}
