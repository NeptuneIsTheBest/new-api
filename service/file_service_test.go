package service

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileCacheStorageLifecycle(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(strconv.FormatBool(disk), func(t *testing.T) {
			tempRoot := t.TempDir()
			t.Setenv("TMPDIR", tempRoot)
			t.Setenv("TMP", tempRoot)
			t.Setenv("TEMP", tempRoot)
			previous := common.GetDiskCacheConfig()
			baseline := common.GetDiskCacheStats()
			common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: disk, ThresholdMB: 0, MaxSizeMB: 1, Path: t.TempDir()})
			t.Cleanup(func() {
				stats := common.GetDiskCacheStats()
				assert.Equal(t, baseline.ActiveDiskFiles, stats.ActiveDiskFiles)
				assert.Equal(t, baseline.CurrentDiskUsageBytes, stats.CurrentDiskUsageBytes)
				assert.Equal(t, baseline.ActiveMemoryBuffers, stats.ActiveMemoryBuffers)
				assert.Equal(t, baseline.CurrentMemoryUsageBytes, stats.CurrentMemoryUsageBytes)
				common.SetDiskCacheConfig(previous)
			})
			var raw bytes.Buffer
			require.NoError(t, png.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 2, 3))))
			encoded := base64.StdEncoding.EncodeToString(raw.Bytes())
			context, _ := gin.CreateTestContext(nil)
			first := types.NewBase64FileSource(encoded, "image/png")
			second := types.NewBase64FileSource(encoded, "image/png")
			cached, err := LoadFileSource(context, first)
			require.NoError(t, err)
			t.Cleanup(func() { CleanupFileSources(context) })
			shared, err := LoadFileSource(context, second)
			require.NoError(t, err)
			assert.Same(t, cached, shared)
			assert.Equal(t, disk, cached.IsDisk())
			assert.EqualValues(t, raw.Len(), cached.Size)
			assert.Equal(t, "image/png", cached.MimeType)
			require.NotNil(t, cached.ImageConfig)
			assert.Equal(t, 2, cached.ImageConfig.Width)
			assert.Equal(t, 3, cached.ImageConfig.Height)
			got, err := cached.GetBase64Data()
			require.NoError(t, err)
			assert.Equal(t, encoded, got)
			stats := common.GetDiskCacheStats()
			if disk {
				assert.EqualValues(t, len(encoded), cached.DiskSize)
				assert.EqualValues(t, baseline.DiskCacheHits+1, stats.DiskCacheHits)
				assert.EqualValues(t, baseline.CurrentDiskUsageBytes+int64(len(encoded)), stats.CurrentDiskUsageBytes)
			} else {
				assert.EqualValues(t, baseline.MemoryCacheHits+1, stats.MemoryCacheHits)
				assert.EqualValues(t, baseline.CurrentMemoryUsageBytes+int64(len(encoded)), stats.CurrentMemoryUsageBytes)
			}
			CleanupFileSources(context)
			CleanupFileSources(context)
			require.NoError(t, cached.Close())
			assert.Zero(t, common.GetDiskCacheDirectoryInfo().FileCount)
		})
	}
}

func TestFileCacheSharesBodyCapacityAndReportsIOErrors(t *testing.T) {
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	t.Setenv("TMP", tempRoot)
	t.Setenv("TEMP", tempRoot)
	previous := common.GetDiskCacheConfig()
	baseline := common.GetDiskCacheStats()
	base := t.TempDir()
	common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: true, MaxSizeMB: 1, Path: base})
	t.Cleanup(func() {
		stats := common.GetDiskCacheStats()
		assert.Equal(t, baseline.CurrentDiskUsageBytes, stats.CurrentDiskUsageBytes)
		assert.Equal(t, baseline.CurrentMemoryUsageBytes, stats.CurrentMemoryUsageBytes)
		common.SetDiskCacheConfig(previous)
	})
	body, err := common.CreateBodyStorage(make([]byte, 1<<20))
	require.NoError(t, err)
	defer body.Close()
	require.True(t, body.IsDisk())
	encoded := base64.StdEncoding.EncodeToString([]byte("file content"))
	cached, err := loadFromBase64(encoded, "application/octet-stream")
	require.NoError(t, err)
	assert.False(t, cached.IsDisk(), "body and file caches must share the same limit")
	got, err := cached.GetBase64Data()
	require.NoError(t, err)
	assert.Equal(t, encoded, got)
	require.NoError(t, cached.Close())
	require.NoError(t, body.Close())

	badDirectory := filepath.Join(base, "regular-file")
	require.NoError(t, os.WriteFile(badDirectory, []byte("not a directory"), 0600))
	config := common.GetDiskCacheConfig()
	config.Path = badDirectory
	common.SetDiskCacheConfig(config)
	cached, err = loadFromBase64(encoded, "application/octet-stream")
	require.Error(t, err)
	assert.False(t, errors.Is(err, common.ErrDiskCacheFull))
	assert.Nil(t, cached, "real I/O failures must not silently fall back")
}

func TestCachedFileDataCleanupCompatibility(t *testing.T) {
	t.Run("legacy disk constructor", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy")
		require.NoError(t, os.WriteFile(path, []byte("encoded"), 0600))
		cached := types.NewDiskCachedData(path, "text/plain", 5)
		cached.DiskSize = 7
		var released int64
		cached.OnClose = func(size int64) { released += size }
		require.NoError(t, cached.Close())
		require.NoError(t, cached.Close())
		assert.EqualValues(t, 7, released)
		assert.NoFileExists(t, path)
	})
	t.Run("host owns cleanup retry", func(t *testing.T) {
		cached := types.NewDiskCachedData("host-owned", "text/plain", 5)
		calls := 0
		cached.Cleanup = func() error {
			calls++
			return os.ErrPermission
		}
		require.ErrorIs(t, cached.Close(), os.ErrPermission)
		require.NoError(t, cached.Close())
		assert.Equal(t, 1, calls)
		_, err := cached.GetBase64Data()
		require.Error(t, err)
	})
	t.Run("memory cleanup is idempotent", func(t *testing.T) {
		cached := types.NewMemoryCachedData(strings.Repeat("x", 5), "text/plain", 5)
		calls := 0
		cached.Cleanup = func() error { calls++; return nil }
		require.NoError(t, cached.Close())
		require.NoError(t, cached.Close())
		assert.Equal(t, 1, calls)
		got, err := cached.GetBase64Data()
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}
