package common

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
)

type DiskCacheConfig struct {
	Enabled       bool
	ThresholdMB   int
	MaxSizeMB     int
	Path          string
	directoryPath string
}

// Config, reservations, file ownership and stats share the manager's lock.
var diskCacheConfig = DiskCacheConfig{ThresholdMB: 10, MaxSizeMB: 1024, directoryPath: resolveDiskCacheDirectory("")}

// Resolve existing ancestors as well as existing cache directories so aliases
// such as /tmp and /private/tmp cannot register or clean the same files twice.
// Resolution happens only when applying configuration, not on stats reads.
func resolveDiskCacheDirectory(path string) string {
	if path == "" {
		path = os.TempDir()
	}
	dir := filepath.Join(path, diskCacheDir)
	if absolute, err := filepath.Abs(dir); err == nil {
		dir = absolute
	}
	ancestor := dir
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
			return filepath.Join(resolved, suffix)
		} else if !os.IsNotExist(err) {
			return dir
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return dir
		}
		suffix = filepath.Join(filepath.Base(ancestor), suffix)
		ancestor = parent
	}
}

func (c DiskCacheConfig) limits() (threshold, capacity int64, valid bool) {
	if c.ThresholdMB < 0 || int64(c.ThresholdMB) > math.MaxInt64>>20 ||
		c.MaxSizeMB <= 0 || int64(c.MaxSizeMB) > math.MaxInt64>>20 {
		return 0, 0, false
	}
	return int64(c.ThresholdMB) << 20, int64(c.MaxSizeMB) << 20, true
}

func GetDiskCacheConfig() DiskCacheConfig {
	diskCache.Lock()
	defer diskCache.Unlock()
	return diskCacheConfig
}

func SetDiskCacheConfig(config DiskCacheConfig) {
	config.directoryPath = resolveDiskCacheDirectory(config.Path)
	diskCache.Lock()
	// Symlink resolution does not normalize case on case-insensitive volumes.
	// Reuse the registered directory identity before discovering any files.
	if diskCache.directories[config.directoryPath] == nil {
		if info, err := os.Stat(config.directoryPath); err == nil {
			for path := range diskCache.directories {
				if registered, err := os.Stat(path); err == nil && os.SameFile(info, registered) {
					config.directoryPath = path
					break
				}
			}
		}
	}
	changed := diskCacheConfig != config
	diskCacheConfig = config
	dir := config.directory()
	var scanErr error
	// Options load individually at startup. Do not adopt the default or an
	// intermediate directory before startup cleanup (or the first cache write)
	// establishes the actual configured directory owned by this instance.
	if diskCache.initialized && config.Enabled {
		if directory := diskCache.directories[dir]; directory == nil || !directory.initialized {
			scanErr = scanDiskCacheDirectory(dir)
		}
	}
	diskCache.Unlock()
	if changed {
		if _, _, valid := config.limits(); !valid {
			SysError("invalid disk cache limits; new cache data will use memory")
		}
	}
	if scanErr != nil {
		SysError(fmt.Sprintf("failed to scan disk cache directory: %v", scanErr))
	}
}

func IsDiskCacheEnabled() bool {
	config := GetDiskCacheConfig()
	_, _, valid := config.limits()
	return config.Enabled && valid
}

func GetDiskCacheThresholdBytes() int64 {
	threshold, _, _ := GetDiskCacheConfig().limits()
	return threshold
}

func GetDiskCacheMaxSizeBytes() int64 {
	_, capacity, _ := GetDiskCacheConfig().limits()
	return capacity
}

func GetDiskCachePath() string { return GetDiskCacheConfig().Path }

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

var diskCacheStats DiskCacheStats

func GetDiskCacheStats() DiskCacheStats {
	diskCache.Lock()
	defer diskCache.Unlock()
	stats := diskCacheStats
	stats.DiskCacheThresholdBytes, stats.DiskCacheMaxBytes, _ = diskCacheConfig.limits()
	return stats
}

// IncrementMemoryBuffers records one successful memory cache creation.
func IncrementMemoryBuffers(size int64) {
	diskCache.Lock()
	defer diskCache.Unlock()
	diskCacheStats.ActiveMemoryBuffers++
	diskCacheStats.CurrentMemoryUsageBytes += size
	diskCacheStats.MemoryCacheHits++
}

func DecrementMemoryBuffers(size int64) {
	diskCache.Lock()
	defer diskCache.Unlock()
	diskCacheStats.ActiveMemoryBuffers--
	diskCacheStats.CurrentMemoryUsageBytes -= size
}

// ResetDiskCacheStats never clears live usage or outstanding reservations.
func ResetDiskCacheStats() {
	diskCache.Lock()
	defer diskCache.Unlock()
	diskCacheStats.DiskCacheHits = 0
	diskCacheStats.MemoryCacheHits = 0
}
