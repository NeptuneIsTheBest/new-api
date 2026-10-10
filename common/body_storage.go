package common

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
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
var ErrStorageClosed = fmt.Errorf("body storage is closed")

// memoryStorage 内存存储实现
type memoryStorage struct {
	data   []byte
	reader *bytes.Reader
	size   int64
	closed bool
	refs   int
	mu     sync.Mutex
}

func newMemoryStorage(data []byte) *memoryStorage {
	size := int64(len(data))
	IncrementMemoryBuffers(size)
	return &memoryStorage{
		data:   data,
		reader: bytes.NewReader(data),
		size:   size,
		refs:   1,
	}
}

func (m *memoryStorage) Read(p []byte) (n int, err error) {
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

func (m *memoryStorage) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		m.data = nil
		m.reader = nil
		m.refs--
		if m.refs == 0 {
			DecrementMemoryBuffers(m.size)
		}
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
	m.refs++
	return &memoryStorageReader{reader: bytes.NewReader(m.data), storage: m}, nil
}

func (m *memoryStorage) Size() int64 {
	return m.size
}

func (m *memoryStorage) IsDisk() bool {
	return false
}

// Each reader owns a reference to the immutable memory buffer, even after the
// storage owner closes. Closing it drops both the reference and the backing data.
type memoryStorageReader struct {
	mu      sync.Mutex
	reader  *bytes.Reader
	storage *memoryStorage
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
	if r.storage == nil {
		return nil
	}
	m := r.storage
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs--
	if m.refs == 0 {
		DecrementMemoryBuffers(m.size)
	}
	r.reader = nil
	r.storage = nil
	return nil
}

// CreateBodyStorage keeps the original data available until disk storage is
// complete, making any write failure safe to fall back from.
func CreateBodyStorage(data []byte) (BodyStorage, error) {
	config := GetDiskCacheConfig()
	threshold, _, valid := config.limits()
	if config.Enabled && valid && int64(len(data)) >= threshold {
		file, err := createDiskCacheFile(DiskCacheTypeBody, int64(len(data)), config)
		if err == nil {
			_, err = file.Write(data)
			if err == nil {
				err = file.Seal()
			}
			if err == nil {
				return file, nil
			}
			file.Close()
		}
		if !errors.Is(err, ErrDiskCacheUnavailable) {
			SysError(fmt.Sprintf("failed to cache request body, falling back to memory: %v", err))
		}
	}
	return newMemoryStorage(data), nil
}

// CreateBodyStorageFromReader spills based on bytes actually received. A length
// header is only a reservation hint; it never substitutes for the size limit.
func CreateBodyStorageFromReader(reader io.Reader, contentLength int64, maxBytes int64) (BodyStorage, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("invalid request body size limit")
	}
	config := GetDiskCacheConfig()
	threshold, capacity, valid := config.limits()
	diskAllowed := config.Enabled && valid && threshold <= min(maxBytes, capacity)
	var disk *DiskCacheFile
	completed := false
	defer func() {
		if disk != nil && !completed {
			disk.Close()
		}
	}()

	if diskAllowed && contentLength >= threshold {
		var err error
		disk, err = createDiskCacheFile(DiskCacheTypeBody, min(contentLength, maxBytes), config)
		if err != nil {
			diskAllowed = false
			if !errors.Is(err, ErrDiskCacheUnavailable) {
				SysError(fmt.Sprintf("failed to create body cache, falling back to memory: %v", err))
			}
		}
	}

	if !diskAllowed && maxBytes < math.MaxInt64 {
		data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxBytes {
			return nil, ErrRequestBodyTooLarge
		}
		return newMemoryStorage(data), nil
	}

	var memory bytes.Buffer
	buffer := diskCacheBuffers.Get().(*[32 * 1024]byte)
	defer func() {
		clear(buffer[:])
		diskCacheBuffers.Put(buffer)
	}()
	var total int64
	for {
		readSize := len(buffer)
		if remaining := maxBytes - total; remaining < int64(readSize) {
			// Probe one extra byte without overflowing maxBytes+1.
			readSize = int(remaining) + 1
		}
		n, readErr := reader.Read(buffer[:readSize])
		if int64(n) > maxBytes-total {
			return nil, ErrRequestBodyTooLarge
		}
		total += int64(n)
		if n > 0 {
			if disk == nil && diskAllowed && total >= threshold {
				var err error
				disk, err = createDiskCacheFile(DiskCacheTypeBody, max(total, min(contentLength, maxBytes)), config)
				if err == nil {
					_, err = disk.Write(memory.Bytes())
					if err == nil {
						memory = bytes.Buffer{}
					} else {
						// The entire prefix is still in memory until promotion succeeds.
						disk.Close()
						disk = nil
					}
				}
				if err != nil {
					diskAllowed = false
					if !errors.Is(err, ErrDiskCacheUnavailable) {
						SysError(fmt.Sprintf("failed to spill body cache, falling back to memory: %v", err))
					}
				}
			}
			if disk != nil {
				prefixSize := disk.Size()
				if _, err := disk.Write(buffer[:n]); err != nil {
					// Exclude any partial write of the current chunk. That complete
					// chunk is still in buffer and must be appended exactly once.
					if _, restoreErr := io.CopyN(&memory, io.NewSectionReader(disk, 0, prefixSize), prefixSize); restoreErr != nil {
						return nil, fmt.Errorf("failed to restore body cache: %w", errors.Join(err, restoreErr))
					}
					memory.Write(buffer[:n])
					disk.Close()
					disk = nil
					diskAllowed = false
					SysError(fmt.Sprintf("body cache write failed, restored to memory: %v", err))
				}
			} else {
				memory.Write(buffer[:n])
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return nil, readErr
			}
			break
		}
	}
	if disk == nil {
		return newMemoryStorage(memory.Bytes()), nil
	}
	if err := disk.Seal(); err != nil {
		data, restoreErr := disk.Bytes()
		if restoreErr != nil {
			return nil, fmt.Errorf("failed to restore body cache: %w", errors.Join(err, restoreErr))
		}
		SysError(fmt.Sprintf("failed to seal body cache, restored to memory: %v", err))
		return newMemoryStorage(data), nil
	}
	completed = true
	return disk, nil
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
