package common

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
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
var ErrStorageClosed = fmt.Errorf("body storage is closed")

// BodyStorageWriter builds an outbound body without buffering the entire payload
// before spilling to disk. Its zero value is ready to use. It is request-local
// and must not be used concurrently. Defer Close to discard an unfinished body;
// Finish transfers ownership of the returned body to the caller.
type BodyStorageWriter struct {
	buffer     bytes.Buffer
	disk       *diskStorage
	memoryOnly bool
	closed     bool
	err        error
}

func (w *BodyStorageWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, ErrStorageClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	if w.disk != nil && !IsDiskCacheAvailable(int64(len(p))) {
		// Capacity can run out while another request is being built. Retain
		// the usual memory fallback, recovering the complete prefix first.
		data, err := w.disk.Bytes()
		if err != nil {
			w.err = err
			_ = w.Close()
			return 0, err
		}
		_ = w.disk.Close()
		w.disk = nil
		w.buffer = *bytes.NewBuffer(data)
		w.memoryOnly = true
	}

	if w.disk == nil && !w.memoryOnly && ShouldUseDiskCache(int64(w.buffer.Len())+int64(len(p))) {
		path, file, err := CreateDiskCacheFile(DiskCacheTypeBody)
		if err != nil {
			SysError(fmt.Sprintf("failed to create outbound body storage, falling back to memory: %v", err))
			w.memoryOnly = true
		} else {
			w.disk = &diskStorage{file: file, filePath: path}
			IncrementDiskFiles(0)
			n, err := file.Write(w.buffer.Bytes())
			w.disk.size = int64(n)
			atomic.AddInt64(&diskCacheStats.CurrentDiskUsageBytes, int64(n))
			if err == nil && n != w.buffer.Len() {
				err = io.ErrShortWrite
			}
			w.buffer = bytes.Buffer{}
			if err != nil {
				w.err = err
				_ = w.Close()
				return 0, err
			}
		}
	}

	if w.disk == nil {
		return w.buffer.Write(p)
	}
	n, err := w.disk.file.Write(p)
	w.disk.size += int64(n)
	atomic.AddInt64(&diskCacheStats.CurrentDiskUsageBytes, int64(n))
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
		_ = w.Close()
	}
	return n, err
}

// Finish returns a *bytes.Buffer for an in-memory body, preserving existing
// adaptor behavior. A disk-backed body is a BodyStorage; its caller must close
// it after the upstream attempt. ApplyUpstreamBodyMetadata keeps net/http from
// closing that storage prematurely and provides independent replay readers.
func (w *BodyStorageWriter) Finish() (io.Reader, error) {
	if w.err != nil {
		return nil, w.err
	}
	if w.closed {
		return nil, ErrStorageClosed
	}
	if w.disk != nil {
		if _, err := w.disk.Seek(0, io.SeekStart); err != nil {
			w.err = err
			_ = w.Close()
			return nil, err
		}
		body := w.disk
		w.disk = nil
		w.closed = true
		return body, nil
	}
	body := bytes.NewBuffer(w.buffer.Bytes())
	w.buffer = bytes.Buffer{}
	w.closed = true
	return body, nil
}

func (w *BodyStorageWriter) Close() error {
	w.closed = true
	w.buffer = bytes.Buffer{}
	if w.disk != nil {
		err := w.disk.Close()
		w.disk = nil
		return err
	}
	return nil
}

// memoryStorage 内存存储实现
type memoryStorage struct {
	data   []byte
	reader *bytes.Reader
	size   int64
	closed int32
	mu     sync.Mutex
}

func newMemoryStorage(data []byte) *memoryStorage {
	size := int64(len(data))
	IncrementMemoryBuffers(size)
	return &memoryStorage{
		data:   data,
		reader: bytes.NewReader(data),
		size:   size,
	}
}

func (m *memoryStorage) Read(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return m.reader.Read(p)
}

func (m *memoryStorage) Seek(offset int64, whence int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return m.reader.Seek(offset, whence)
}

func (m *memoryStorage) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.CompareAndSwapInt32(&m.closed, 0, 1) {
		DecrementMemoryBuffers(m.size)
		// Existing replay readers retain their own references to the payload.
		m.data = nil
		m.reader = nil
	}
	return nil
}

func (m *memoryStorage) Bytes() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return nil, ErrStorageClosed
	}
	return m.data, nil
}

func (m *memoryStorage) NewReader() (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return nil, ErrStorageClosed
	}
	// A fresh bytes.Reader over the shared immutable backing array: an
	// independent cursor at zero copy cost. NopCloser keeps Close a no-op, so
	// the storage lifecycle stays owned by whoever holds the storage itself.
	return io.NopCloser(bytes.NewReader(m.data)), nil
}

func (m *memoryStorage) Size() int64 {
	return m.size
}

func (m *memoryStorage) IsDisk() bool {
	return false
}

// diskStorage 磁盘存储实现
type diskStorage struct {
	file     *os.File
	filePath string
	size     int64
	closed   int32
	mu       sync.Mutex
}

func newDiskStorage(data []byte, cachePath string) (*diskStorage, error) {
	// 使用统一的缓存目录管理
	filePath, file, err := CreateDiskCacheFile(DiskCacheTypeBody)
	if err != nil {
		return nil, err
	}

	// 写入数据
	n, err := file.Write(data)
	if err != nil {
		file.Close()
		os.Remove(filePath)
		return nil, fmt.Errorf("failed to write to temp file: %w", err)
	}

	// 重置文件指针
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		os.Remove(filePath)
		return nil, fmt.Errorf("failed to seek temp file: %w", err)
	}

	size := int64(n)
	IncrementDiskFiles(size)

	return &diskStorage{
		file:     file,
		filePath: filePath,
		size:     size,
	}, nil
}

func newDiskStorageFromReader(reader io.Reader, maxBytes int64, prefix []byte) (*diskStorage, error) {
	if int64(len(prefix)) > maxBytes {
		return nil, ErrRequestBodyTooLarge
	}
	// 使用统一的缓存目录管理
	filePath, file, err := CreateDiskCacheFile(DiskCacheTypeBody)
	if err != nil {
		return nil, err
	}

	// Write the buffered prefix once instead of copying it in small chunks.
	n, err := file.Write(prefix)
	written := int64(n)
	if err == nil {
		var copied int64
		copied, err = io.Copy(file, io.LimitReader(reader, maxBytes+1-written))
		written += copied
	}
	if err != nil {
		file.Close()
		os.Remove(filePath)
		return nil, fmt.Errorf("failed to write to temp file: %w", err)
	}

	if written > maxBytes {
		file.Close()
		os.Remove(filePath)
		return nil, ErrRequestBodyTooLarge
	}

	// 重置文件指针
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		os.Remove(filePath)
		return nil, fmt.Errorf("failed to seek temp file: %w", err)
	}

	IncrementDiskFiles(written)

	return &diskStorage{
		file:     file,
		filePath: filePath,
		size:     written,
	}, nil
}

func (d *diskStorage) Read(p []byte) (n int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return d.file.Read(p)
}

func (d *diskStorage) Seek(offset int64, whence int) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return d.file.Seek(offset, whence)
}

func (d *diskStorage) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.CompareAndSwapInt32(&d.closed, 0, 1) {
		d.file.Close()
		os.Remove(d.filePath)
		DecrementDiskFiles(d.size)
	}
	return nil
}

func (d *diskStorage) Bytes() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if atomic.LoadInt32(&d.closed) == 1 {
		return nil, ErrStorageClosed
	}

	// Read at a fixed offset so the replay cursor stays unchanged even when
	// a truncated cache file or another read error prevents a complete read.
	data := make([]byte, d.size)
	n, err := d.file.ReadAt(data, 0)
	if err == io.EOF && n > 0 {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (d *diskStorage) NewReader() (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) == 1 {
		return nil, ErrStorageClosed
	}
	// A separate file descriptor over the same cache file: an independent
	// cursor at zero copy cost. Closing the returned reader closes only that
	// descriptor; the storage keeps owning the primary descriptor and the
	// file's lifetime. Readers opened before Close stay usable even after the
	// file is unlinked, as the descriptor keeps the inode alive.
	file, err := os.Open(d.filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open body cache file for replay: %w", err)
	}
	return file, nil
}

func (d *diskStorage) Size() int64 {
	return d.size
}

func (d *diskStorage) IsDisk() bool {
	return true
}

// CreateBodyStorage 根据数据大小创建合适的存储
func CreateBodyStorage(data []byte) (BodyStorage, error) {
	size := int64(len(data))
	threshold := GetDiskCacheThresholdBytes()

	// 检查是否应该使用磁盘缓存
	if IsDiskCacheEnabled() &&
		size >= threshold &&
		IsDiskCacheAvailable(size) {
		storage, err := newDiskStorage(data, GetDiskCachePath())
		if err != nil {
			// 如果磁盘存储失败，回退到内存存储
			SysError(fmt.Sprintf("failed to create disk storage, falling back to memory: %v", err))
			return newMemoryStorage(data), nil
		}
		return storage, nil
	}

	return newMemoryStorage(data), nil
}

// CreateBodyStorageFromReader 从 Reader 创建存储（用于大请求的流式处理）
func CreateBodyStorageFromReader(reader io.Reader, contentLength int64, maxBytes int64) (BodyStorage, error) {
	threshold := GetDiskCacheThresholdBytes()
	diskSize := contentLength
	var prefix []byte

	// Unknown or understated lengths (e.g. compressed requests) must not force
	// the entire body through memory before spilling. Require room for the full
	// request limit because the eventual size is not known yet.
	if IsDiskCacheEnabled() && threshold <= maxBytes &&
		(diskSize <= 0 || diskSize < threshold) && IsDiskCacheAvailable(maxBytes) {
		var err error
		prefix, err = io.ReadAll(io.LimitReader(reader, max(threshold, 0)))
		if err != nil {
			return nil, err
		}
		if int64(len(prefix)) < threshold {
			IncrementMemoryCacheHits()
			return newMemoryStorage(prefix), nil
		}
		diskSize = maxBytes
	}

	// 已知大请求及达到阈值的请求直接流式写入磁盘。
	if IsDiskCacheEnabled() &&
		diskSize > 0 &&
		diskSize >= threshold &&
		IsDiskCacheAvailable(diskSize) {
		storage, err := newDiskStorageFromReader(reader, maxBytes, prefix)
		if err != nil {
			if IsRequestBodyTooLargeError(err) {
				return nil, err
			}
			// 磁盘存储失败，reader 已被消费，无法安全回退
			// 直接返回错误而非尝试回退（因为 reader 数据已丢失）
			return nil, fmt.Errorf("disk storage creation failed: %w", err)
		}
		IncrementDiskCacheHits()
		return storage, nil
	}

	// 使用内存读取
	if len(prefix) > 0 {
		reader = io.MultiReader(bytes.NewReader(prefix), reader)
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrRequestBodyTooLarge
	}

	storage, err := CreateBodyStorage(data)
	if err != nil {
		return nil, err
	}
	// 如果最终使用内存存储，记录内存缓存命中
	if !storage.IsDisk() {
		IncrementMemoryCacheHits()
	} else {
		IncrementDiskCacheHits()
	}
	return storage, nil
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

// CleanupOldCacheFiles 清理旧的缓存文件（用于启动时清理残留）
func CleanupOldCacheFiles() {
	// 使用统一的缓存管理
	CleanupOldDiskCacheFiles(5 * time.Minute)
}
