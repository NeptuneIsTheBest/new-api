package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

type DiskCacheType string

const (
	DiskCacheTypeBody DiskCacheType = "body"
	DiskCacheTypeFile DiskCacheType = "file"
	diskCacheDir                    = "new-api-body-cache"
)

var ErrDiskCacheUnavailable = errors.New("disk cache capacity is unavailable")

var diskCacheBuffers = sync.Pool{New: func() any { return new([32 * 1024]byte) }}

// Accept both CreateTemp names and the UUID/timestamp names used before it.
var diskCacheFileName = regexp.MustCompile(`^(body|file)-([0-9]+|[0-9a-f]{8}-[0-9]+)\.tmp$`)

type diskCacheEntry struct {
	path          string
	directory     *diskCacheDirectory
	size          int64
	reserved      int64
	refs          int
	info          os.FileInfo
	pendingDelete bool
}

type diskCacheDirectory struct {
	DiskCacheDirectoryInfo
	initialized bool
}

// DiskCacheDirectoryInfo describes managed files in the configured directory.
// Usage in directories retained after a config change is included in cache stats.
type DiskCacheDirectoryInfo struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	FileCount int    `json:"file_count"`
	TotalSize int64  `json:"total_size"`
}

var diskCache = struct {
	sync.Mutex
	files       map[string]*diskCacheEntry
	directories map[string]*diskCacheDirectory
	reserved    int64
	initialized bool
}{
	files:       make(map[string]*diskCacheEntry),
	directories: make(map[string]*diskCacheDirectory),
}

func (c DiskCacheConfig) directory() string {
	return c.directoryPath
}

func GetDiskCacheDir() string { return GetDiskCacheConfig().directory() }

// scanDiskCacheDirectory reconciles inactive files while holding diskCache's
// lock. Active files can be in the middle of a write and retain their accounting.
// Scans run on startup, directory changes, and cleanup, never on stats reads.
func scanDiskCacheDirectory(dir string) error {
	directory := diskCache.directories[dir]
	if directory == nil {
		directory = &diskCacheDirectory{DiskCacheDirectoryInfo: DiskCacheDirectoryInfo{Path: dir}}
		diskCache.directories[dir] = directory
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	directory.Exists = err == nil
	seen := make(map[string]bool, len(entries))
	var scanErr error
	for _, item := range entries {
		if !diskCacheFileName.MatchString(item.Name()) || !item.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, item.Name())
		seen[path] = true
		entry := diskCache.files[path]
		if entry != nil && entry.refs > 0 {
			continue
		}
		info, err := item.Info()
		if err != nil {
			scanErr = errors.Join(scanErr, err)
			continue
		}
		if !info.Mode().IsRegular() {
			delete(seen, path)
			continue
		}
		if entry == nil {
			entry = &diskCacheEntry{path: path, directory: directory}
			diskCache.files[path] = entry
			directory.FileCount++
		}
		if entry.info != nil && !os.SameFile(entry.info, info) {
			entry.pendingDelete = false
		}
		delta := info.Size() - entry.size
		directory.TotalSize += delta
		diskCacheStats.CurrentDiskUsageBytes += delta
		entry.size = info.Size()
		entry.info = info
	}
	for path, entry := range diskCache.files {
		if entry.directory == directory && entry.refs == 0 && !seen[path] {
			forgetDiskCacheEntry(entry)
		}
	}
	directory.initialized = scanErr == nil
	return scanErr
}

// forgetDiskCacheEntry requires diskCache's lock and an entry with no owners.
func forgetDiskCacheEntry(entry *diskCacheEntry) {
	delete(diskCache.files, entry.path)
	entry.directory.FileCount--
	entry.directory.TotalSize -= entry.size
	diskCacheStats.CurrentDiskUsageBytes -= entry.size
}

// Failed deletion leaves the file charged and eligible for a later retry.
func removeDiskCacheEntry(entry *diskCacheEntry) error {
	info, err := os.Lstat(entry.path)
	if os.IsNotExist(err) {
		forgetDiskCacheEntry(entry)
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(entry.info, info) {
		return fmt.Errorf("cache file was replaced: %s", entry.path)
	}
	if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	forgetDiskCacheEntry(entry)
	return nil
}

// Requires diskCache's lock. Reservations cover I/O in progress, so releasing
// the manager lock during disk writes cannot admit excess data.
func reserveDiskCacheBytes(size int64) bool {
	_, limit, valid := diskCacheConfig.limits()
	used := diskCacheStats.CurrentDiskUsageBytes
	if !diskCacheConfig.Enabled || !valid || size < 0 || used > limit || diskCache.reserved > limit-used {
		return false
	}
	if size > limit-used-diskCache.reserved {
		return false
	}
	diskCache.reserved += size
	return true
}

// DiskCacheFile owns a quota-accounted cache file. Seal completes a successful
// write; Close releases ownership. Replay readers retain the file until closed.
// The underlying os.File is private so writes cannot bypass quota.
type DiskCacheFile struct {
	mu       sync.Mutex
	file     *os.File
	entry    *diskCacheEntry
	sealed   bool
	closed   bool
	closeErr error
}

// CreateDiskCacheFile reserves a known-size payload using one config snapshot.
// Payloads below the disk threshold should be stored in memory instead.
func CreateDiskCacheFile(cacheType DiskCacheType, expectedSize int64) (*DiskCacheFile, error) {
	config := GetDiskCacheConfig()
	threshold, _, valid := config.limits()
	if !config.Enabled || !valid || expectedSize < threshold {
		return nil, ErrDiskCacheUnavailable
	}
	return createDiskCacheFile(cacheType, expectedSize, config)
}

func createDiskCacheFile(cacheType DiskCacheType, expectedSize int64, config DiskCacheConfig) (*DiskCacheFile, error) {
	if cacheType != DiskCacheTypeBody && cacheType != DiskCacheTypeFile {
		return nil, fmt.Errorf("invalid disk cache type: %s", cacheType)
	}
	if _, _, valid := config.limits(); !config.Enabled || !valid || expectedSize < 0 {
		return nil, ErrDiskCacheUnavailable
	}
	diskCache.Lock()
	defer diskCache.Unlock()
	diskCache.initialized = true
	dir := config.directory()
	directory := diskCache.directories[dir]
	if directory == nil || !directory.initialized {
		if err := scanDiskCacheDirectory(dir); err != nil {
			return nil, err
		}
		directory = diskCache.directories[dir]
	}
	if !reserveDiskCacheBytes(expectedSize) {
		return nil, ErrDiskCacheUnavailable
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		diskCache.reserved -= expectedSize
		return nil, err
	}
	directory.Exists = true
	file, err := os.CreateTemp(dir, string(cacheType)+"-*.tmp")
	if err != nil {
		diskCache.reserved -= expectedSize
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		os.Remove(file.Name())
		diskCache.reserved -= expectedSize
		return nil, err
	}
	entry := &diskCacheEntry{path: file.Name(), directory: directory, reserved: expectedSize, refs: 1, info: info}
	diskCache.files[entry.path] = entry
	directory.FileCount++
	diskCacheStats.ActiveDiskFiles++
	return &DiskCacheFile{file: file, entry: entry}, nil
}

func (f *DiskCacheFile) Path() string { return f.entry.path }

func (f *DiskCacheFile) Size() int64 {
	diskCache.Lock()
	defer diskCache.Unlock()
	return f.entry.size
}

func (f *DiskCacheFile) IsDisk() bool { return true }

func (f *DiskCacheFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.sealed {
		return 0, fmt.Errorf("cache file is not writable")
	}
	diskCache.Lock()
	extra := max(int64(len(p))-f.entry.reserved, 0)
	if extra > 0 && !reserveDiskCacheBytes(extra) {
		diskCache.Unlock()
		return 0, ErrDiskCacheUnavailable
	}
	f.entry.reserved += extra
	diskCache.Unlock()

	// Always append, even after seeking to inspect a partially written file.
	n, err := f.file.WriteAt(p, f.entry.size)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	diskCache.Lock()
	f.entry.reserved -= int64(n)
	diskCache.reserved -= int64(n)
	f.entry.size += int64(n)
	f.entry.directory.TotalSize += int64(n)
	diskCacheStats.CurrentDiskUsageBytes += int64(n)
	diskCache.Unlock()
	return n, err
}

// WriteString avoids a payload-sized []byte allocation.
func (f *DiskCacheFile) WriteString(s string) (int, error) {
	buffer := diskCacheBuffers.Get().(*[32 * 1024]byte)
	defer func() {
		clear(buffer[:])
		diskCacheBuffers.Put(buffer)
	}()
	written := 0
	for len(s) > 0 {
		n := copy(buffer[:], s)
		count, err := f.Write(buffer[:n])
		written += count
		if err != nil {
			return written, err
		}
		s = s[n:]
	}
	return written, nil
}

func (f *DiskCacheFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, ErrStorageClosed
	}
	return f.file.Read(p)
}

func (f *DiskCacheFile) ReadAt(p []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, ErrStorageClosed
	}
	return f.file.ReadAt(p, offset)
}

func (f *DiskCacheFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, ErrStorageClosed
	}
	return f.file.Seek(offset, whence)
}

func (f *DiskCacheFile) Bytes() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrStorageClosed
	}
	data := make([]byte, f.entry.size)
	_, err := io.ReadFull(io.NewSectionReader(f.file, 0, f.entry.size), data)
	return data, err
}

// Seal releases unused reservations and records one successful cache creation.
func (f *DiskCacheFile) Seal() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrStorageClosed
	}
	if f.sealed {
		return nil
	}
	if _, err := f.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	f.sealed = true
	diskCache.Lock()
	diskCache.reserved -= f.entry.reserved
	f.entry.reserved = 0
	diskCacheStats.DiskCacheHits++
	diskCache.Unlock()
	return nil
}

func (f *DiskCacheFile) NewReader() (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrStorageClosed
	}
	if !f.sealed {
		return nil, fmt.Errorf("cache file has not been sealed")
	}
	file, err := os.Open(f.entry.path)
	if err != nil {
		return nil, err
	}
	diskCache.Lock()
	f.entry.refs++
	diskCache.Unlock()
	return &diskCacheReader{file: file, entry: f.entry}, nil
}

func (f *DiskCacheFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		f.closeErr = errors.Join(f.file.Close(), releaseDiskCacheEntry(f.entry))
	}
	return f.closeErr
}

type diskCacheReader struct {
	file  *os.File
	entry *diskCacheEntry
	once  sync.Once
	err   error
}

func (r *diskCacheReader) Read(p []byte) (int, error) { return r.file.Read(p) }

func (r *diskCacheReader) Close() error {
	r.once.Do(func() {
		r.err = errors.Join(r.file.Close(), releaseDiskCacheEntry(r.entry))
	})
	return r.err
}

func releaseDiskCacheEntry(entry *diskCacheEntry) error {
	diskCache.Lock()
	defer diskCache.Unlock()
	entry.refs--
	if entry.refs > 0 {
		return nil
	}
	diskCacheStats.ActiveDiskFiles--
	diskCache.reserved -= entry.reserved
	entry.reserved = 0
	entry.pendingDelete = true
	return removeDiskCacheEntry(entry)
}

// CleanupOldDiskCacheFiles reconciles all directories used by this process.
// An active owner or replay reader always takes precedence over file age.
func CleanupOldDiskCacheFiles(maxAge time.Duration) error {
	diskCache.Lock()
	defer diskCache.Unlock()
	diskCache.initialized = true
	dir := diskCacheConfig.directory()
	if diskCache.directories[dir] == nil {
		diskCache.directories[dir] = &diskCacheDirectory{DiskCacheDirectoryInfo: DiskCacheDirectoryInfo{Path: dir}}
	}
	var cleanupErr error
	for path, directory := range diskCache.directories {
		if path != dir && directory.FileCount == 0 {
			delete(diskCache.directories, path)
			continue
		}
		cleanupErr = errors.Join(cleanupErr, scanDiskCacheDirectory(path))
	}
	now := time.Now()
	for _, entry := range diskCache.files {
		if entry.refs > 0 || entry.info == nil {
			continue
		}
		if entry.pendingDelete || now.Sub(entry.info.ModTime()) > maxAge {
			cleanupErr = errors.Join(cleanupErr, removeDiskCacheEntry(entry))
		}
	}
	return cleanupErr
}

func GetDiskCacheDirectoryInfo() DiskCacheDirectoryInfo {
	diskCache.Lock()
	defer diskCache.Unlock()
	dir := diskCacheConfig.directory()
	if directory := diskCache.directories[dir]; directory != nil {
		return directory.DiskCacheDirectoryInfo
	}
	return DiskCacheDirectoryInfo{Path: dir}
}

func GetDiskCacheInfo() (fileCount int, totalSize int64, err error) {
	info := GetDiskCacheDirectoryInfo()
	return info.FileCount, info.TotalSize, nil
}

func ShouldUseDiskCache(dataSize int64) bool {
	diskCache.Lock()
	defer diskCache.Unlock()
	threshold, limit, valid := diskCacheConfig.limits()
	used := diskCacheStats.CurrentDiskUsageBytes
	return diskCacheConfig.Enabled && valid && dataSize >= threshold &&
		used <= limit && diskCache.reserved <= limit-used && dataSize <= limit-used-diskCache.reserved
}

func CleanupOldCacheFiles() {
	if err := CleanupOldDiskCacheFiles(10 * time.Minute); err != nil {
		SysError(fmt.Sprintf("failed to clean disk cache: %v", err))
	}
}

// RunDiskCacheCleanup stops with the process lifecycle context.
func RunDiskCacheCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			CleanupOldCacheFiles()
		}
	}
}
