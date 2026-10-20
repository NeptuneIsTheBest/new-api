package common

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// Cache configuration tests are sequential. Isolate the manager as well as
// the directory so cleanup cannot visit a developer's default cache directory.
func useDiskCache(t *testing.T, threshold, capacity int) string {
	t.Helper()
	diskCache.mu.Lock()
	previous := diskCacheManager{
		initialized: diskCache.initialized,
		config:      diskCache.config,
		stats:       diskCache.stats,
		reserved:    diskCache.reserved,
		files:       diskCache.files,
		directories: diskCache.directories,
	}
	diskCache.initialized = false
	diskCache.stats = DiskCacheStats{}
	diskCache.reserved = 0
	diskCache.files = make(map[string]*diskCacheEntry)
	diskCache.directories = make(map[string]*diskCacheDirectory)
	diskCache.mu.Unlock()
	base := t.TempDir()
	SetDiskCacheConfig(DiskCacheConfig{Enabled: true, ThresholdMB: threshold, MaxSizeMB: capacity, Path: base})
	t.Cleanup(func() {
		stats := GetDiskCacheStats()
		assert.Zero(t, stats.ActiveDiskFiles, "disk owners must be released")
		assert.Zero(t, stats.CurrentDiskUsageBytes, "disk usage must be released")
		assert.Zero(t, stats.ActiveMemoryBuffers, "memory owners must be released")
		assert.Zero(t, stats.CurrentMemoryUsageBytes, "memory usage must be released")
		diskCache.mu.Lock()
		diskCache.initialized = previous.initialized
		diskCache.config = previous.config
		diskCache.stats = previous.stats
		diskCache.reserved = previous.reserved
		diskCache.files = previous.files
		diskCache.directories = previous.directories
		diskCache.mu.Unlock()
	})
	return base
}

type observedCacheReader struct {
	reader io.Reader
	before func()
}

func (r *observedCacheReader) Read(p []byte) (int, error) {
	r.before()
	return r.reader.Read(p)
}

func TestBodyStorageStreaming(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int
		length    int64
		threshold int
		capacity  int
		disk      bool
	}{
		{"below threshold", (1 << 20) - 1, -1, 1, 4, false},
		{"at threshold", 1 << 20, -1, 1, 4, true},
		{"unknown length", (2 << 20) + 13, -1, 1, 4, true},
		{"understated length", (2 << 20) + 13, 1, 1, 4, true},
		{"known length", (2 << 20) + 13, (2 << 20) + 13, 1, 4, true},
		{"overstated length", 31, 2 << 20, 1, 4, true},
		{"unknown length quota fallback", (2 << 20) + 13, -1, 1, 1, false},
		{"known length quota fallback", (2 << 20) + 13, (2 << 20) + 13, 1, 1, false},
		{"zero threshold", 31, -1, 0, 4, true},
		{"empty", 0, -1, 1, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useDiskCache(t, tc.threshold, tc.capacity)
			payload := bytes.Repeat([]byte("abcdefghijk"), tc.size/11+1)[:tc.size]
			storage, err := CreateBodyStorageFromReader(bytes.NewReader(payload), tc.length, int64(tc.size))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, storage.Close()) })
			assert.Equal(t, tc.disk, storage.IsDisk())
			assert.EqualValues(t, len(payload), storage.Size())
			got, err := io.ReadAll(storage)
			require.NoError(t, err)
			assert.Equal(t, payload, got, "fallback must preserve both the prefix and the remaining stream")
			got, err = storage.Bytes()
			require.NoError(t, err)
			assert.Equal(t, payload, got)
		})
	}
}

func TestBodyStorageSpillsBeforeEOF(t *testing.T) {
	useDiskCache(t, 1, 4)
	payload := strings.Repeat("x", 3<<20)
	sawDiskDuringRead := false
	reader := &observedCacheReader{
		reader: strings.NewReader(payload),
		before: func() {
			if GetDiskCacheStats().CurrentDiskUsageBytes > 0 {
				sawDiskDuringRead = true
				assert.LessOrEqual(t, GetDiskCacheStats().CurrentDiskUsageBytes, int64(len(payload)))
			}
		},
	}
	storage, err := CreateBodyStorageFromReader(reader, -1, int64(len(payload)))
	require.NoError(t, err)
	defer storage.Close()
	assert.True(t, sawDiskDuringRead, "unknown-length input must spill before the final read")
	assert.True(t, storage.IsDisk())
}

type failedCacheReader struct {
	err error
}

func (r failedCacheReader) Read([]byte) (int, error) { return 0, r.err }

func TestBodyStorageReadFailuresReleaseCapacity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader io.Reader
		limit  int64
		want   error
	}{
		{"oversize", strings.NewReader(strings.Repeat("a", (1<<20)+1)), 1 << 20, ErrRequestBodyTooLarge},
		{"read failure after writing", io.MultiReader(strings.NewReader(strings.Repeat("b", 1<<20)), failedCacheReader{io.ErrUnexpectedEOF}), 2 << 20, io.ErrUnexpectedEOF},
		{"zero limit", strings.NewReader("x"), 0, ErrRequestBodyTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useDiskCache(t, 0, 1)
			storage, err := CreateBodyStorageFromReader(tc.reader, -1, tc.limit)
			require.ErrorIs(t, err, tc.want)
			assert.Nil(t, storage)
			assert.Zero(t, GetDiskCacheStats().CurrentDiskUsageBytes)
			info := GetDiskCacheDirectoryInfo()
			assert.Zero(t, info.FileCount)
			replacement, err := CreateBodyStorage(make([]byte, 1<<20))
			require.NoError(t, err)
			defer replacement.Close()
			assert.True(t, replacement.IsDisk(), "failed reads must return all reservations")
		})
	}
	t.Run("maximum int64 limit", func(t *testing.T) {
		useDiskCache(t, 0, 1)
		storage, err := CreateBodyStorageFromReader(strings.NewReader("payload"), -1, math.MaxInt64)
		require.NoError(t, err)
		defer storage.Close()
		got, err := storage.Bytes()
		require.NoError(t, err)
		assert.Equal(t, "payload", string(got))
	})
}

func TestDiskCacheConcurrentCapacity(t *testing.T) {
	useDiskCache(t, 0, 1)
	payload := bytes.Repeat([]byte("x"), 600<<10)
	type result struct {
		storage BodyStorage
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 3)
	var workers sync.WaitGroup
	for range 3 {
		workers.Go(func() {
			<-start
			storage, err := CreateBodyStorage(payload)
			results <- result{storage, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	disks := 0
	for result := range results {
		require.NoError(t, result.err)
		storage := result.storage
		t.Cleanup(func() { require.NoError(t, storage.Close()) })
		if storage.IsDisk() {
			disks++
		}
		got, err := storage.Bytes()
		require.NoError(t, err)
		assert.Equal(t, payload, got)
	}
	assert.Equal(t, 1, disks)
	stats := GetDiskCacheStats()
	assert.EqualValues(t, len(payload), stats.CurrentDiskUsageBytes)
	assert.EqualValues(t, 1, stats.ActiveDiskFiles)
	assert.EqualValues(t, 2, stats.ActiveMemoryBuffers)
}

func TestCacheReadersRetainStorageUntilLastClose(t *testing.T) {
	for _, useDisk := range []bool{false, true} {
		t.Run(strconv.FormatBool(useDisk), func(t *testing.T) {
			useDiskCache(t, 0, 1)
			config := GetDiskCacheConfig()
			config.Enabled = useDisk
			SetDiskCacheConfig(config)
			payload := []byte("independent replay payload")
			storage, err := CreateBodyStorage(payload)
			require.NoError(t, err)
			reader, err := storage.NewReader()
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, storage.Close())
				require.NoError(t, reader.Close())
			})
			if useDisk {
				entries, err := os.ReadDir(GetDiskCacheDir())
				require.NoError(t, err)
				require.Len(t, entries, 1)
				old := time.Now().Add(-time.Hour)
				require.NoError(t, os.Chtimes(filepath.Join(GetDiskCacheDir(), entries[0].Name()), old, old))
			}
			require.NoError(t, CleanupOldDiskCacheFiles(10*time.Minute))
			partial := make([]byte, 5)
			_, err = io.ReadFull(storage, partial)
			require.NoError(t, err)
			all, err := storage.Bytes()
			require.NoError(t, err)
			assert.Equal(t, payload, all)
			rest, err := io.ReadAll(storage)
			require.NoError(t, err)
			assert.Equal(t, payload[5:], rest, "Bytes must not change the primary cursor")
			require.NoError(t, storage.Close())
			require.NoError(t, storage.Close())
			_, err = storage.NewReader()
			require.ErrorIs(t, err, ErrStorageClosed)
			require.NoError(t, CleanupOldDiskCacheFiles(0))
			stats := GetDiskCacheStats()
			if useDisk {
				assert.EqualValues(t, 1, stats.ActiveDiskFiles)
				assert.EqualValues(t, len(payload), stats.CurrentDiskUsageBytes)
			} else {
				assert.EqualValues(t, 1, stats.ActiveMemoryBuffers)
				assert.EqualValues(t, len(payload), stats.CurrentMemoryUsageBytes)
			}
			got, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Equal(t, payload, got)
			require.NoError(t, reader.Close())
			require.NoError(t, reader.Close())
		})
	}
}

func TestDiskCacheConcurrentCleanupAndRelease(t *testing.T) {
	useDiskCache(t, 0, 1)
	payload := []byte("replay survives concurrent cleanup")
	storage, err := CreateBodyStorage(payload)
	require.NoError(t, err)
	reader, err := storage.NewReader()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, storage.Close())
		require.NoError(t, reader.Close())
	})
	start := make(chan struct{})
	results := make(chan error, 3)
	var replay []byte
	var workers sync.WaitGroup
	workers.Go(func() { <-start; results <- storage.Close() })
	workers.Go(func() {
		<-start
		SyncDiskCacheStats()
		ResetDiskCacheStats()
		results <- CleanupOldDiskCacheFiles(-time.Second)
	})
	workers.Go(func() {
		<-start
		var readErr error
		replay, readErr = io.ReadAll(reader)
		results <- errors.Join(readErr, reader.Close())
	})
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	assert.Equal(t, payload, replay)
	assert.Zero(t, GetDiskCacheDirectoryInfo().FileCount)
}

func TestDiskCacheFailedWritesReleaseReservations(t *testing.T) {
	for _, stringWrite := range []bool{false, true} {
		t.Run(strconv.FormatBool(stringWrite), func(t *testing.T) {
			useDiskCache(t, 0, 1)
			path, file, err := CreateDiskCacheFile(DiskCacheTypeFile)
			require.NoError(t, err)
			// Closing the OS descriptor injects a real write error without
			// replacing the filesystem or relying on permissions/root identity.
			require.NoError(t, file.file.Close())
			if stringWrite {
				_, err = file.WriteString("failed write")
			} else {
				_, err = file.Write([]byte("failed write"))
			}
			require.ErrorIs(t, err, os.ErrClosed)
			require.ErrorIs(t, file.Close(), os.ErrClosed)
			assert.NoFileExists(t, path)
			storage, err := CreateBodyStorage(make([]byte, 1<<20))
			require.NoError(t, err)
			defer storage.Close()
			assert.True(t, storage.IsDisk())
		})
	}
}

func TestDiskCacheDeleteFailureRetainsUsage(t *testing.T) {
	useDiskCache(t, 0, 1)
	path, err := WriteDiskCacheFileString(DiskCacheTypeFile, "retained")
	require.NoError(t, err)
	// Replace the pathname with a directory to make removal fail on all
	// supported platforms, including tests running as root.
	held := path + ".held"
	require.NoError(t, os.Rename(path, held))
	require.NoError(t, os.Mkdir(path, 0700))
	err = RemoveDiskCacheFile(path)
	require.Error(t, err)
	stats := GetDiskCacheStats()
	assert.Zero(t, stats.ActiveDiskFiles)
	assert.EqualValues(t, len("retained"), stats.CurrentDiskUsageBytes)
	require.NoError(t, RemoveDiskCacheFile(path))
	SyncDiskCacheStats()
	assert.Equal(t, stats.CurrentDiskUsageBytes, GetDiskCacheStats().CurrentDiskUsageBytes)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Rename(held, path))
	require.NoError(t, CleanupOldDiskCacheFiles(-time.Second))
	assert.NoFileExists(t, path)
	assert.Zero(t, GetDiskCacheStats().CurrentDiskUsageBytes)
}

func TestDiskCacheOrphanCleanup(t *testing.T) {
	base := useDiskCache(t, 0, 1)
	config := GetDiskCacheConfig()
	config.Path = filepath.Join(base, "startup")
	dir := filepath.Join(config.Path, diskCacheDir)
	require.NoError(t, os.MkdirAll(dir, 0700))
	old := filepath.Join(dir, "body-deadbeef-1.tmp")
	recent := filepath.Join(dir, "file-decafbad-2.tmp")
	unrelated := filepath.Join(dir, "body-not-a-cache.tmp")
	target := filepath.Join(base, "target")
	link := filepath.Join(dir, "body-deadbeef-3.tmp")
	for _, path := range []string{old, recent, unrelated, target} {
		require.NoError(t, os.WriteFile(path, []byte("abc"), 0600))
	}
	require.NoError(t, os.Symlink(target, link))
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(old, past, past))
	SetDiskCacheConfig(config)
	SyncDiskCacheStats()
	stats := GetDiskCacheStats()
	assert.EqualValues(t, 6, stats.CurrentDiskUsageBytes)
	assert.Zero(t, stats.ActiveDiskFiles)
	assert.Equal(t, 2, GetDiskCacheDirectoryInfo().FileCount)
	require.NoError(t, CleanupOldDiskCacheFiles(10*time.Minute))
	assert.NoFileExists(t, old)
	assert.FileExists(t, recent)
	assert.FileExists(t, unrelated)
	assert.FileExists(t, target)
	_, err := os.Lstat(link)
	require.NoError(t, err)
	require.NoError(t, CleanupOldDiskCacheFiles(-time.Second))
	assert.Zero(t, GetDiskCacheStats().CurrentDiskUsageBytes)
}

func TestDiskCacheConfigChangesKeepExistingOwners(t *testing.T) {
	useDiskCache(t, 0, 2)
	payload := bytes.Repeat([]byte("x"), 1500<<10)
	storage, err := CreateBodyStorage(payload)
	require.NoError(t, err)
	defer storage.Close()
	reader, err := storage.NewReader()
	require.NoError(t, err)
	defer reader.Close()

	config := GetDiskCacheConfig()
	config.Path = t.TempDir()
	config.MaxSizeMB = 1
	SetDiskCacheConfig(config)
	assert.EqualValues(t, len(payload), GetDiskCacheStats().CurrentDiskUsageBytes)
	assert.Zero(t, GetDiskCacheDirectoryInfo().FileCount)
	require.NoError(t, CleanupOldDiskCacheFiles(0))
	replacement, err := CreateBodyStorage([]byte("small"))
	require.NoError(t, err)
	assert.False(t, replacement.IsDisk(), "shrinking the limit must prevent new reservations")
	require.NoError(t, replacement.Close())
	require.NoError(t, storage.Close())
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	require.NoError(t, reader.Close())
	replacement, err = CreateBodyStorage([]byte("small"))
	require.NoError(t, err)
	defer replacement.Close()
	assert.True(t, replacement.IsDisk())
	assert.Equal(t, 1, GetDiskCacheDirectoryInfo().FileCount)
}

func TestDiskCacheInvalidConfigurationAndStatsReset(t *testing.T) {
	useDiskCache(t, 0, 1)
	config := GetDiskCacheConfig()
	invalid := []DiskCacheConfig{
		{Enabled: true, ThresholdMB: -1, MaxSizeMB: 1, Path: config.Path},
		{Enabled: true, MaxSizeMB: 0, Path: config.Path},
		{Enabled: true, MaxSizeMB: -1, Path: config.Path},
	}
	if strconv.IntSize == 64 {
		overflow := int(^uint(0) >> 1)
		invalid = append(invalid,
			DiskCacheConfig{Enabled: true, ThresholdMB: overflow, MaxSizeMB: 1, Path: config.Path},
			DiskCacheConfig{Enabled: true, MaxSizeMB: overflow, Path: config.Path},
		)
	}
	for _, config := range invalid {
		SetDiskCacheConfig(config)
		assert.False(t, IsDiskCacheAvailable(1))
		storage, err := CreateBodyStorage([]byte("x"))
		require.NoError(t, err)
		assert.False(t, storage.IsDisk())
		require.NoError(t, storage.Close())
	}
	SetDiskCacheConfig(config)
	assert.False(t, IsDiskCacheAvailable(-1))
	assert.False(t, IsDiskCacheAvailable(math.MaxInt64))
	before := GetDiskCacheStats()
	storage, err := CreateBodyStorage([]byte("x"))
	require.NoError(t, err)
	defer storage.Close()
	path, err := WriteDiskCacheFileString(DiskCacheTypeFile, "file")
	require.NoError(t, err)
	defer RemoveDiskCacheFile(path)
	stats := GetDiskCacheStats()
	assert.EqualValues(t, before.DiskCacheHits+2, stats.DiskCacheHits)
	ResetDiskCacheStats()
	ResetDiskCacheUsage()
	after := GetDiskCacheStats()
	assert.Zero(t, after.DiskCacheHits)
	assert.Zero(t, after.MemoryCacheHits)
	assert.Equal(t, stats.ActiveDiskFiles, after.ActiveDiskFiles)
	assert.Equal(t, stats.CurrentDiskUsageBytes, after.CurrentDiskUsageBytes)
}

func TestDiskCacheIOErrorsDoNotFallback(t *testing.T) {
	base := useDiskCache(t, 0, 1)
	blocker := filepath.Join(base, "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0600))
	config := GetDiskCacheConfig()
	config.Path = blocker
	SetDiskCacheConfig(config)
	storage, err := CreateBodyStorage([]byte("payload"))
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrDiskCacheFull))
	assert.Nil(t, storage)
	storage, err = CreateBodyStorageFromReader(strings.NewReader("payload"), -1, 100)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrDiskCacheFull))
	assert.Nil(t, storage)
}

func TestBodyStorageQuotaReductionDuringRead(t *testing.T) {
	useDiskCache(t, 0, 2)
	config := GetDiskCacheConfig()
	payload := strings.Repeat("changing quota ", 100000)
	changed := false
	reader := &observedCacheReader{
		reader: strings.NewReader(payload),
		before: func() {
			if !changed && GetDiskCacheStats().CurrentDiskUsageBytes > 0 {
				changed = true
				config.MaxSizeMB = 1
				SetDiskCacheConfig(config)
			}
		},
	}
	storage, err := CreateBodyStorageFromReader(reader, int64(len(payload)), int64(len(payload)))
	require.NoError(t, err)
	defer storage.Close()
	assert.True(t, changed)
	assert.False(t, storage.IsDisk())
	got, err := storage.Bytes()
	require.NoError(t, err)
	assert.Equal(t, payload, string(got))
	assert.Zero(t, GetDiskCacheStats().CurrentDiskUsageBytes)
}

func TestDiskCacheStartupUsesFinalConfiguredDirectory(t *testing.T) {
	base := useDiskCache(t, 0, 1)
	transient := GetDiskCacheDir()
	require.NoError(t, os.MkdirAll(transient, 0700))
	foreign := filepath.Join(transient, "body-deadbeef-1.tmp")
	require.NoError(t, os.WriteFile(foreign, []byte("another instance"), 0600))
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(foreign, past, past))
	config := GetDiskCacheConfig()
	config.Path = filepath.Join(base, "persisted")
	SetDiskCacheConfig(config)
	CleanupOldCacheFiles()
	assert.FileExists(t, foreign, "startup must not adopt a transient configuration's directory")
	assert.Zero(t, GetDiskCacheStats().CurrentDiskUsageBytes)
}
