package common

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"time"
)

// BodyStorage 请求体存储接口
type BodyStorage interface {
	io.ReadSeeker
	io.Closer
	// Bytes 获取全部内容
	Bytes() ([]byte, error)
	// Size 获取数据大小
	Size() int64
	// IsDisk 是否是磁盘存储
	IsDisk() bool
	// NewReader returns an independent reader positioned at the start of the
	// stored payload. Each call returns a reader with its own cursor, so
	// callers (e.g. http.Request.GetBody) can replay the body concurrently
	// with, or after, other readers without sharing seek state. Closing the
	// returned reader releases only that reader, never the storage itself;
	// after the storage has been closed, NewReader returns ErrStorageClosed.
	NewReader() (io.ReadCloser, error)
}

// ReplayableBody is an outbound request body that can report its byte size and
// create independent readers for transport-level retries.
type ReplayableBody interface {
	io.Reader
	Size() int64
	NewReader() (io.ReadCloser, error)
}

// ErrStorageClosed 存储已关闭错误
var ErrStorageClosed = errors.New("body storage is closed")

type memoryStorage struct {
	data       []byte
	reader     *bytes.Reader
	size       int64
	closed     bool
	references int
	mu         sync.Mutex
}

func newMemoryStorage(data []byte) *memoryStorage {
	size := int64(len(data))
	IncrementMemoryBuffers(size)
	IncrementMemoryCacheHits()
	return &memoryStorage{data: data, reader: bytes.NewReader(data), size: size, references: 1}
}

func (m *memoryStorage) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, ErrStorageClosed
	}
	return m.reader.Read(p)
}

func (m *memoryStorage) Seek(offset int64, whence int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, ErrStorageClosed
	}
	return m.reader.Seek(offset, whence)
}

func (m *memoryStorage) releaseLocked() {
	m.references--
	if m.references == 0 {
		DecrementMemoryBuffers(m.size)
	}
}

func (m *memoryStorage) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		m.data = nil
		m.reader = nil
		m.releaseLocked()
	}
	return nil
}

func (m *memoryStorage) Bytes() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrStorageClosed
	}
	return m.data, nil
}

func (m *memoryStorage) NewReader() (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrStorageClosed
	}
	m.references++
	return &memoryStorageReader{reader: bytes.NewReader(m.data), storage: m}, nil
}

func (m *memoryStorage) Size() int64  { return m.size }
func (m *memoryStorage) IsDisk() bool { return false }

type memoryStorageReader struct {
	reader  *bytes.Reader
	storage *memoryStorage
	mu      sync.Mutex
}

func (r *memoryStorageReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reader == nil {
		return 0, ErrStorageClosed
	}
	return r.reader.Read(p)
}

func (r *memoryStorageReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reader != nil {
		r.reader = nil
		r.storage.mu.Lock()
		r.storage.releaseLocked()
		r.storage.mu.Unlock()
	}
	return nil
}

type diskStorage struct {
	file   *DiskCacheFile
	size   int64
	closed bool
	mu     sync.Mutex
}

func (d *diskStorage) Read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, ErrStorageClosed
	}
	return d.file.file.Read(p)
}

func (d *diskStorage) Seek(offset int64, whence int) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, ErrStorageClosed
	}
	return d.file.file.Seek(offset, whence)
}

func (d *diskStorage) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return d.file.Close()
}

func (d *diskStorage) Bytes() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, ErrStorageClosed
	}
	// ReadAt leaves the primary cursor unchanged even when a read fails.
	data := make([]byte, d.size)
	_, err := d.file.file.ReadAt(data, 0)
	return data, err
}

func (d *diskStorage) NewReader() (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, ErrStorageClosed
	}
	return d.file.newReader()
}

func (d *diskStorage) Size() int64  { return d.size }
func (d *diskStorage) IsDisk() bool { return true }

func CreateBodyStorage(data []byte) (BodyStorage, error) {
	config := GetDiskCacheConfig()
	threshold, _, valid := config.limits()
	if !config.Enabled || !valid || int64(len(data)) < threshold || !IsDiskCacheAvailable(int64(len(data))) {
		return newMemoryStorage(data), nil
	}
	file, err := createDiskCacheFile(DiskCacheTypeBody, config)
	if errors.Is(err, ErrDiskCacheFull) {
		return newMemoryStorage(data), nil
	}
	if err != nil {
		return nil, err
	}
	n, err := file.Write(data)
	if err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, closeErr
		}
		if errors.Is(err, ErrDiskCacheFull) {
			return newMemoryStorage(data), nil
		}
		return nil, err
	}
	if _, err := file.file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	IncrementDiskCacheHits()
	return &diskStorage{file: file, size: int64(n)}, nil
}

// CreateBodyStorageFromReader bounds reads by actual bytes. Content-Length is
// only a hint: unknown and understated lengths also spill as they grow.
func CreateBodyStorageFromReader(reader io.Reader, contentLength, maxBytes int64) (_ BodyStorage, resultErr error) {
	if maxBytes < 0 {
		return nil, ErrRequestBodyTooLarge
	}
	config := GetDiskCacheConfig()
	threshold, _, valid := config.limits()
	allowDisk := config.Enabled && valid
	var memory bytes.Buffer
	var disk *diskStorage
	defer func() {
		if resultErr != nil && disk != nil {
			resultErr = errors.Join(resultErr, disk.Close())
		}
	}()

	if allowDisk && (threshold == 0 || contentLength > 0 && contentLength >= threshold) {
		file, err := createDiskCacheFile(DiskCacheTypeBody, config)
		if errors.Is(err, ErrDiskCacheFull) {
			allowDisk = false
		} else if err != nil {
			return nil, err
		} else {
			disk = &diskStorage{file: file}
		}
	}

	// Keep the overflow probe bounded without computing maxBytes+1.
	buffer := make([]byte, min(maxBytes, 64*1024-1)+1)
	var total int64
	emptyReads := 0
	for {
		readSize := len(buffer)
		if remaining := maxBytes - total; remaining < int64(readSize) {
			readSize = int(remaining) + 1
		}
		n, readErr := reader.Read(buffer[:readSize])
		if n > 0 {
			emptyReads = 0
			if int64(n) > maxBytes-total {
				return nil, ErrRequestBodyTooLarge
			}
			total += int64(n)
			if disk == nil && allowDisk && total >= threshold {
				file, err := createDiskCacheFile(DiskCacheTypeBody, config)
				if errors.Is(err, ErrDiskCacheFull) {
					allowDisk = false
				} else if err != nil {
					return nil, err
				} else {
					disk = &diskStorage{file: file}
					written, err := file.Write(memory.Bytes())
					disk.size = int64(written)
					if errors.Is(err, ErrDiskCacheFull) {
						if err := disk.Close(); err != nil {
							return nil, err
						}
						disk = nil
						allowDisk = false
					} else if err != nil {
						return nil, err
					} else {
						memory = bytes.Buffer{}
					}
				}
			}
			if disk != nil {
				written, err := disk.file.Write(buffer[:n])
				disk.size += int64(written)
				if errors.Is(err, ErrDiskCacheFull) {
					prefix, err := disk.Bytes()
					if err != nil {
						return nil, err
					}
					memory = *bytes.NewBuffer(prefix)
					if err := disk.Close(); err != nil {
						return nil, err
					}
					disk = nil
					allowDisk = false
					memory.Write(buffer[:n])
				} else if err != nil {
					return nil, err
				}
			} else {
				memory.Write(buffer[:n])
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return nil, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
			break
		}
	}

	if disk != nil {
		if _, err := disk.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		IncrementDiskCacheHits()
		return disk, nil
	}
	data := memory.Bytes()
	if data == nil {
		data = []byte{}
	}
	return newMemoryStorage(data), nil
}

type replayableBodyReader struct {
	storage BodyStorage
}

func (r replayableBodyReader) Read(p []byte) (int, error) {
	return r.storage.Read(p)
}

func (r replayableBodyReader) Size() int64 {
	return r.storage.Size()
}

func (r replayableBodyReader) NewReader() (io.ReadCloser, error) {
	return r.storage.NewReader()
}

// NewReplayableBodyReader exposes the replay capabilities of storage without
// exposing io.Closer. This keeps ownership of the storage lifecycle with the
// caller instead of allowing net/http to close it as the request body.
func NewReplayableBodyReader(storage BodyStorage) ReplayableBody {
	return replayableBodyReader{storage: storage}
}

// CleanupOldCacheFiles reclaims startup leftovers without touching active files.
func CleanupOldCacheFiles() {
	if err := CleanupOldDiskCacheFiles(5 * time.Minute); err != nil {
		SysError(LogText("failed to clean up disk cache: %v", err))
	}
}
