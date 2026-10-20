package common

import (
	"math"
	"os"
	"path/filepath"
)

// DiskCacheConfig is updated by performance_setting.
type DiskCacheConfig struct {
	Enabled     bool
	ThresholdMB int
	MaxSizeMB   int
	Path        string
}

func (c DiskCacheConfig) limits() (threshold, capacity int64, valid bool) {
	if c.ThresholdMB < 0 || c.MaxSizeMB <= 0 ||
		int64(c.ThresholdMB) > math.MaxInt64>>20 || int64(c.MaxSizeMB) > math.MaxInt64>>20 {
		return 0, 0, false
	}
	return int64(c.ThresholdMB) << 20, int64(c.MaxSizeMB) << 20, true
}

func (c DiskCacheConfig) directory() string {
	path := c.Path
	if path == "" {
		path = os.TempDir()
	}
	dir := filepath.Join(path, diskCacheDir)
	if absolute, err := filepath.Abs(dir); err == nil {
		return absolute
	}
	return dir
}

func GetDiskCacheConfig() DiskCacheConfig {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	return diskCache.config
}

func SetDiskCacheConfig(config DiskCacheConfig) {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	previousDir := diskCache.config.directory()
	diskCache.config = config
	dir := config.directory()
	// Startup loads settings before opening the cache. Do not scan the
	// transient default directory before the persisted path has been loaded.
	if diskCache.initialized && (dir != previousDir || diskCache.directories[dir] == nil) {
		// Retain any scan error so admission fails explicitly instead of
		// accepting files with an unknown starting usage.
		_ = diskCache.scanDirectoryLocked(dir)
	}
}

func IsDiskCacheEnabled() bool {
	return GetDiskCacheConfig().Enabled
}

func GetDiskCacheThresholdBytes() int64 {
	threshold, _, _ := GetDiskCacheConfig().limits()
	return threshold
}

func GetDiskCacheMaxSizeBytes() int64 {
	_, capacity, _ := GetDiskCacheConfig().limits()
	return capacity
}

func GetDiskCachePath() string {
	return GetDiskCacheConfig().Path
}

type DiskCacheStats struct {
	ActiveDiskFiles         int64 `json:"active_disk_files"`
	CurrentDiskUsageBytes   int64 `json:"current_disk_usage_bytes"`
	ActiveMemoryBuffers     int64 `json:"active_memory_buffers"`
	CurrentMemoryUsageBytes int64 `json:"current_memory_usage_bytes"`
	DiskCacheHits           int64 `json:"disk_cache_hits"`
	MemoryCacheHits         int64 `json:"memory_cache_hits"`
	DiskCacheMaxBytes       int64 `json:"disk_cache_max_bytes"`
	DiskCacheThresholdBytes int64 `json:"disk_cache_threshold_bytes"`
}

func GetDiskCacheStats() DiskCacheStats {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	stats := diskCache.stats
	stats.DiskCacheThresholdBytes, stats.DiskCacheMaxBytes, _ = diskCache.config.limits()
	return stats
}

func IncrementMemoryBuffers(size int64) {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	diskCache.stats.ActiveMemoryBuffers++
	diskCache.stats.CurrentMemoryUsageBytes += size
}

func DecrementMemoryBuffers(size int64) {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	diskCache.stats.ActiveMemoryBuffers--
	diskCache.stats.CurrentMemoryUsageBytes -= size
}

func IncrementDiskCacheHits() {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	diskCache.stats.DiskCacheHits++
}

func IncrementMemoryCacheHits() {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	diskCache.stats.MemoryCacheHits++
}

// ResetDiskCacheStats only resets cumulative selections, never live usage.
func ResetDiskCacheStats() {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	diskCache.stats.DiskCacheHits = 0
	diskCache.stats.MemoryCacheHits = 0
}

// ResetDiskCacheUsage reconciles instead of discarding outstanding ownership.
func ResetDiskCacheUsage() {
	SyncDiskCacheStats()
}

func SyncDiskCacheStats() {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	diskCache.initialized = true
	current := diskCache.config.directory()
	if diskCache.directories[current] == nil {
		diskCache.directories[current] = &diskCacheDirectory{}
	}
	for dir := range diskCache.directories {
		_ = diskCache.scanDirectoryLocked(dir)
	}
}

func IsDiskCacheAvailable(size int64) bool {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	return diskCache.config.Enabled && diskCache.canReserveLocked(size)
}
