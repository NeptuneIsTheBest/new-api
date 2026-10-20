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

	"github.com/google/uuid"
)

type DiskCacheType string

const (
	DiskCacheTypeBody DiskCacheType = "body"
	DiskCacheTypeFile DiskCacheType = "file"
	diskCacheDir                    = "new-api-body-cache"
)

// ErrDiskCacheFull is recoverable by retaining the complete payload in memory.
var ErrDiskCacheFull = errors.New("disk cache capacity exhausted")

var diskCacheName = regexp.MustCompile(`^(body|file)-[0-9a-f]{8}-[0-9]+\.tmp$`)

type diskCacheEntry struct {
	path       string
	size       int64
	references int
	owned      bool
	inactiveAt time.Time
}

type diskCacheDirectory struct {
	exists bool
	count  int
	size   int64
	err    error
}

// The directory is exclusive to this process. Metadata operations and deletion
// are serialized; payload writes reserve space under the lock but do I/O outside it.
type diskCacheManager struct {
	mu          sync.Mutex
	initialized bool
	config      DiskCacheConfig
	stats       DiskCacheStats
	reserved    int64
	files       map[string]*diskCacheEntry
	directories map[string]*diskCacheDirectory
}

var diskCache = diskCacheManager{
	config:      DiskCacheConfig{ThresholdMB: 10, MaxSizeMB: 1024},
	files:       make(map[string]*diskCacheEntry),
	directories: make(map[string]*diskCacheDirectory),
}

func GetDiskCacheDir() string {
	return GetDiskCacheConfig().directory()
}

func EnsureDiskCacheDir() error {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	return diskCache.ensureDirectoryLocked(diskCache.config.directory())
}

func (m *diskCacheManager) ensureDirectoryLocked(dir string) error {
	m.initialized = true
	directory := m.directories[dir]
	if directory == nil || directory.err != nil {
		if err := m.scanDirectoryLocked(dir); err != nil {
			return err
		}
		directory = m.directories[dir]
	}
	if !directory.exists {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("failed to create cache directory: %w", err)
		}
		directory.exists = true
	}
	return nil
}

// DiskCacheFile is an append-only cache writer. Closing it releases its owner;
// independent readers keep the file and its capacity alive until they close.
type DiskCacheFile struct {
	file   *os.File
	entry  *diskCacheEntry
	mu     sync.Mutex
	closed bool
}

func CreateDiskCacheFile(cacheType DiskCacheType) (string, *DiskCacheFile, error) {
	file, err := createDiskCacheFile(cacheType, GetDiskCacheConfig())
	if err != nil {
		return "", nil, err
	}
	return file.entry.path, file, nil
}

func createDiskCacheFile(cacheType DiskCacheType, config DiskCacheConfig) (*DiskCacheFile, error) {
	if cacheType != DiskCacheTypeBody && cacheType != DiskCacheTypeFile {
		return nil, fmt.Errorf("invalid disk cache type: %q", cacheType)
	}
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	if _, _, valid := config.limits(); !config.Enabled || !valid || !diskCache.config.Enabled {
		return nil, ErrDiskCacheFull
	}
	dir := config.directory()
	if err := diskCache.ensureDirectoryLocked(dir); err != nil {
		return nil, err
	}
	if !diskCache.canReserveLocked(0) {
		return nil, ErrDiskCacheFull
	}
	name := fmt.Sprintf("%s-%s-%d.tmp", cacheType, uuid.New().String()[:8], time.Now().UnixNano())
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to create cache file: %w", err)
	}
	entry := &diskCacheEntry{path: path, references: 1, owned: true}
	diskCache.files[path] = entry
	diskCache.directories[dir].count++
	diskCache.stats.ActiveDiskFiles++
	return &DiskCacheFile{file: file, entry: entry}, nil
}

func (m *diskCacheManager) canReserveLocked(size int64) bool {
	_, limit, valid := m.config.limits()
	used := m.stats.CurrentDiskUsageBytes
	return valid && size >= 0 && used <= limit &&
		m.reserved <= limit-used && size <= limit-used-m.reserved
}

func (f *DiskCacheFile) reserve(size int64) error {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	if !diskCache.canReserveLocked(size) {
		return ErrDiskCacheFull
	}
	diskCache.reserved += size
	return nil
}

func (f *DiskCacheFile) settle(reserved int64, written int) {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	diskCache.reserved -= reserved
	f.entry.size += int64(written)
	diskCache.stats.CurrentDiskUsageBytes += int64(written)
	diskCache.directories[filepath.Dir(f.entry.path)].size += int64(written)
}

func (f *DiskCacheFile) Write(data []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, ErrStorageClosed
	}
	if err := f.reserve(int64(len(data))); err != nil {
		return 0, err
	}
	n, err := f.file.Write(data)
	f.settle(int64(len(data)), n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (f *DiskCacheFile) WriteString(data string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, ErrStorageClosed
	}
	if err := f.reserve(int64(len(data))); err != nil {
		return 0, err
	}
	n, err := f.file.WriteString(data)
	f.settle(int64(len(data)), n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (f *DiskCacheFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return errors.Join(f.file.Close(), RemoveDiskCacheFile(f.entry.path))
}

// retainPath transfers ownership to the path-based file cache after closing the
// writer descriptor. Its host cleanup hook must later call RemoveDiskCacheFile.
func (f *DiskCacheFile) retainPath() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrStorageClosed
	}
	f.closed = true
	if err := f.file.Close(); err != nil {
		return errors.Join(err, RemoveDiskCacheFile(f.entry.path))
	}
	IncrementDiskCacheHits()
	return nil
}

func WriteDiskCacheFile(cacheType DiskCacheType, data []byte) (string, error) {
	path, file, err := CreateDiskCacheFile(cacheType)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return "", closeErr
		}
		return "", err
	}
	if err := file.retainPath(); err != nil {
		return "", err
	}
	return path, nil
}

func WriteDiskCacheFileString(cacheType DiskCacheType, data string) (string, error) {
	path, file, err := CreateDiskCacheFile(cacheType)
	if err != nil {
		return "", err
	}
	if _, err := file.WriteString(data); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return "", closeErr
		}
		return "", err
	}
	if err := file.retainPath(); err != nil {
		return "", err
	}
	return path, nil
}

func ReadDiskCacheFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func ReadDiskCacheFileString(path string) (string, error) {
	data, err := ReadDiskCacheFile(path)
	return string(data), err
}

// RemoveDiskCacheFile releases the owner exactly once. It never removes an
// untracked path or a file still held by a replay reader.
func RemoveDiskCacheFile(path string) error {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	entry := diskCache.files[path]
	if entry == nil || !entry.owned {
		return nil
	}
	entry.owned = false
	return diskCache.releaseLocked(entry)
}

func (m *diskCacheManager) releaseLocked(entry *diskCacheEntry) error {
	entry.references--
	if entry.references > 0 {
		return nil
	}
	m.stats.ActiveDiskFiles--
	entry.inactiveAt = time.Now()
	return m.removeLocked(entry)
}

func (m *diskCacheManager) removeLocked(entry *diskCacheEntry) error {
	info, err := os.Lstat(entry.path)
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("cache path is no longer a regular file: %s", entry.path)
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	m.forgetLocked(entry)
	return nil
}

func (m *diskCacheManager) forgetLocked(entry *diskCacheEntry) {
	delete(m.files, entry.path)
	m.stats.CurrentDiskUsageBytes -= entry.size
	directory := m.directories[filepath.Dir(entry.path)]
	directory.count--
	directory.size -= entry.size
}

type diskCacheReader struct {
	file  *os.File
	entry *diskCacheEntry
	once  sync.Once
	err   error
}

func (r *diskCacheReader) Read(p []byte) (int, error) {
	return r.file.Read(p)
}

func (r *diskCacheReader) Close() error {
	r.once.Do(func() {
		closeErr := r.file.Close()
		diskCache.mu.Lock()
		defer diskCache.mu.Unlock()
		r.err = errors.Join(closeErr, diskCache.releaseLocked(r.entry))
	})
	return r.err
}

func (f *DiskCacheFile) newReader() (io.ReadCloser, error) {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	if !f.entry.owned {
		return nil, ErrStorageClosed
	}
	file, err := os.Open(f.entry.path)
	if err != nil {
		return nil, fmt.Errorf("failed to open body cache file for replay: %w", err)
	}
	f.entry.references++
	return &diskCacheReader{file: file, entry: f.entry}, nil
}

// scanDirectoryLocked only reconciles inactive files; an active file may be
// between its disk write and accounting commit. Never overwrite that accounting.
func (m *diskCacheManager) scanDirectoryLocked(dir string) error {
	directory := m.directories[dir]
	if directory == nil {
		directory = &diskCacheDirectory{}
		m.directories[dir] = directory
	}
	entries, err := os.ReadDir(dir)
	directory.err = err
	if os.IsNotExist(err) {
		directory.err = nil
		directory.exists = false
	} else if err != nil {
		return err
	} else {
		directory.exists = true
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !diskCacheName.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			seen[path] = true
			directory.err = errors.Join(directory.err, err)
			continue
		}
		if !info.Mode().IsRegular() {
			// A failed removal may leave a tracked pathname temporarily
			// inaccessible or replaced. Do not release its charge on a scan.
			seen[path] = m.files[path] != nil
			continue
		}
		seen[path] = true
		cached := m.files[path]
		if cached == nil {
			cached = &diskCacheEntry{path: path, inactiveAt: info.ModTime()}
			m.files[path] = cached
			directory.count++
		}
		if cached.references == 0 {
			change := info.Size() - cached.size
			cached.size = info.Size()
			m.stats.CurrentDiskUsageBytes += change
			directory.size += change
		}
	}
	for path, entry := range m.files {
		if filepath.Dir(path) == dir && entry.references == 0 && !seen[path] {
			m.forgetLocked(entry)
		}
	}
	return directory.err
}

func CleanupOldDiskCacheFiles(maxAge time.Duration) error {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	diskCache.initialized = true
	var result error
	current := diskCache.config.directory()
	if diskCache.directories[current] == nil {
		diskCache.directories[current] = &diskCacheDirectory{}
	}
	for dir := range diskCache.directories {
		result = errors.Join(result, diskCache.scanDirectoryLocked(dir))
	}
	now := time.Now()
	for _, entry := range diskCache.files {
		if entry.references == 0 && now.Sub(entry.inactiveAt) > maxAge {
			result = errors.Join(result, diskCache.removeLocked(entry))
		}
	}
	for dir, directory := range diskCache.directories {
		if dir != current && directory.count == 0 && directory.err == nil {
			delete(diskCache.directories, dir)
		}
	}
	return result
}

// DiskCacheDirectoryInfo is a cached snapshot, not a filesystem scan.
type DiskCacheDirectoryInfo struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	FileCount int    `json:"file_count"`
	TotalSize int64  `json:"total_size"`
}

func GetDiskCacheDirectoryInfo() DiskCacheDirectoryInfo {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	info := DiskCacheDirectoryInfo{Path: diskCache.config.directory()}
	if directory := diskCache.directories[info.Path]; directory != nil {
		info.Exists = directory.exists
		info.FileCount = directory.count
		info.TotalSize = directory.size
	}
	return info
}

func GetDiskCacheInfo() (fileCount int, totalSize int64, err error) {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	if directory := diskCache.directories[diskCache.config.directory()]; directory != nil {
		return directory.count, directory.size, directory.err
	}
	return 0, 0, nil
}

func ShouldUseDiskCache(size int64) bool {
	diskCache.mu.Lock()
	defer diskCache.mu.Unlock()
	threshold, _, valid := diskCache.config.limits()
	return diskCache.config.Enabled && valid && size >= threshold && diskCache.canReserveLocked(size)
}

// RunDiskCacheCleanup runs independently of performance monitoring and stops
// with the application's context.
func RunDiskCacheCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := CleanupOldDiskCacheFiles(10 * time.Minute); err != nil {
				SysError(LogText("failed to clean up disk cache: %v", err))
			}
		}
	}
}
