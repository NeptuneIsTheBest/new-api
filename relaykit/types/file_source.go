package types

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"strings"
	"sync"
)

// FileSource 统一的文件来源抽象接口
// 支持 URL 和 base64 两种来源，提供懒加载和缓存机制
type FileSource interface {
	IsURL() bool
	GetIdentifier() string
	GetRawData() string
	ClearRawData()

	SetCache(data *CachedFileData)
	GetCache() *CachedFileData
	HasCache() bool
	ClearCache()

	IsRegistered() bool
	SetRegistered(registered bool)
	Mu() *sync.Mutex
}

// baseFileSource 共享的缓存/锁/清理注册状态
type baseFileSource struct {
	cachedData  *CachedFileData
	cacheLoaded bool
	registered  bool
	mu          sync.Mutex
}

func (b *baseFileSource) SetCache(data *CachedFileData) {
	b.cachedData = data
	b.cacheLoaded = true
}

func (b *baseFileSource) GetCache() *CachedFileData {
	return b.cachedData
}

func (b *baseFileSource) HasCache() bool {
	return b.cacheLoaded && b.cachedData != nil
}

func (b *baseFileSource) ClearCache() {
	if b.cachedData != nil {
		b.cachedData.Close()
	}
	b.cachedData = nil
	b.cacheLoaded = false
}

func (b *baseFileSource) IsRegistered() bool {
	return b.registered
}

func (b *baseFileSource) SetRegistered(registered bool) {
	b.registered = registered
}

func (b *baseFileSource) Mu() *sync.Mutex {
	return &b.mu
}

// ---------------------------------------------------------------------------
// URLSource — URL 来源的 FileSource 实现
// ---------------------------------------------------------------------------

type URLSource struct {
	baseFileSource
	URL string
}

func (u *URLSource) IsURL() bool { return true }

func (u *URLSource) GetIdentifier() string {
	if len(u.URL) > 100 {
		return u.URL[:100] + "..."
	}
	return u.URL
}

func (u *URLSource) GetRawData() string { return u.URL }

func (u *URLSource) ClearRawData() {}

// ---------------------------------------------------------------------------
// Base64Source — Base64 内联数据来源的 FileSource 实现
// ---------------------------------------------------------------------------

type Base64Source struct {
	baseFileSource
	Base64Data string
	MimeType   string
}

func (b *Base64Source) IsURL() bool { return false }

func (b *Base64Source) GetIdentifier() string {
	if len(b.Base64Data) > 50 {
		return "base64:" + b.Base64Data[:50] + "..."
	}
	return "base64:" + b.Base64Data
}

func (b *Base64Source) GetRawData() string { return b.Base64Data }

func (b *Base64Source) ClearRawData() {
	if len(b.Base64Data) > 1024 {
		b.Base64Data = ""
	}
}

// ---------------------------------------------------------------------------
// Constructors
// ---------------------------------------------------------------------------

func NewURLFileSource(url string) *URLSource {
	return &URLSource{URL: url}
}

func NewBase64FileSource(base64Data string, mimeType string) *Base64Source {
	return &Base64Source{
		Base64Data: base64Data,
		MimeType:   mimeType,
	}
}

func NewFileSourceFromData(data string, mimeType string) FileSource {
	if strings.HasPrefix(data, "http://") || strings.HasPrefix(data, "https://") {
		return NewURLFileSource(data)
	}
	return NewBase64FileSource(data, mimeType)
}

// ---------------------------------------------------------------------------
// CachedFileData — 缓存的文件数据（支持内存和磁盘两种模式）
// ---------------------------------------------------------------------------

type CachedFileData struct {
	base64Data  string        // 内存中的 base64 数据（小文件）
	MimeType    string        // MIME 类型
	Size        int64         // 文件大小（字节）
	DiskSize    int64         // 磁盘缓存实际占用大小（字节，通常是 base64 长度）
	ImageConfig *image.Config // 图片配置（如果是图片）
	ImageFormat string        // 图片格式（如果是图片）

	diskPath        string     // 磁盘缓存文件路径（大文件）
	isDisk          bool       // 是否使用磁盘缓存
	diskMu          sync.Mutex // 磁盘操作锁（保护磁盘文件的读取和删除）
	diskClosed      bool       // 是否已关闭/清理
	statDecremented bool       // 是否已扣减统计
	diskReaders     int        // 删除文件前等待独立 reader 关闭

	OnClose func(size int64)
}

func NewMemoryCachedData(base64Data string, mimeType string, size int64) *CachedFileData {
	return &CachedFileData{
		base64Data: base64Data,
		MimeType:   mimeType,
		Size:       size,
		isDisk:     false,
	}
}

func NewDiskCachedData(diskPath string, mimeType string, size int64) *CachedFileData {
	return &CachedFileData{
		diskPath: diskPath,
		MimeType: mimeType,
		Size:     size,
		isDisk:   true,
	}
}

func (c *CachedFileData) GetBase64Data() (string, error) {
	if !c.isDisk {
		c.diskMu.Lock()
		defer c.diskMu.Unlock()
		return c.base64Data, nil
	}

	reader, err := c.OpenBase64Reader()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	info, err := reader.(*cachedFileReader).file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat disk cache: %w", err)
	}
	var data strings.Builder
	data.Grow(int(info.Size()))
	if _, err := io.Copy(&data, reader); err != nil {
		return "", fmt.Errorf("failed to read from disk cache: %w", err)
	}
	return data.String(), nil
}

// OpenBase64Reader returns an independent reader of the encoded cache. Closing
// it releases only the reader. Readers opened before Close remain usable until
// they close; the last reader then removes the closed cache's file.
func (c *CachedFileData) OpenBase64Reader() (io.ReadCloser, error) {
	c.diskMu.Lock()
	defer c.diskMu.Unlock()
	if !c.isDisk {
		return io.NopCloser(strings.NewReader(c.base64Data)), nil
	}
	if c.diskClosed {
		return nil, fmt.Errorf("disk cache already closed")
	}
	file, err := os.Open(c.diskPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read from disk cache: %w", err)
	}
	c.diskReaders++
	return &cachedFileReader{file: file, cache: c}, nil
}

// OpenDecodedReader streams the original bytes and owns its encoded reader.
func (c *CachedFileData) OpenDecodedReader() (io.ReadCloser, error) {
	reader, err := c.OpenBase64Reader()
	if err != nil {
		return nil, err
	}
	return &decodedFileReader{
		Reader: base64.NewDecoder(base64.StdEncoding, &base64PaddingReader{reader: reader}),
		Closer: reader,
	}, nil
}

// The standard stream decoder decodes independent blocks and can otherwise
// accept more data after a padded block. Keep DecodeString's whole-input rule.
type base64PaddingReader struct {
	reader io.Reader
	padded bool
	offset int64
}

func (r *base64PaddingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	start := 0
	if !r.padded {
		start = bytes.IndexByte(p[:n], '=')
		if start < 0 {
			r.offset += int64(n)
			return n, err
		}
		r.padded = true
	}
	for i := start; i < n; i++ {
		if p[i] != '=' && p[i] != '\r' && p[i] != '\n' {
			return i, base64.CorruptInputError(r.offset + int64(i))
		}
	}
	r.offset += int64(n)
	return n, err
}

type decodedFileReader struct {
	io.Reader
	io.Closer
}

type cachedFileReader struct {
	file  *os.File
	cache *CachedFileData
	once  sync.Once
	err   error
}

func (r *cachedFileReader) Read(p []byte) (int, error) {
	return r.file.Read(p)
}

func (r *cachedFileReader) Close() error {
	r.once.Do(func() {
		r.err = r.file.Close()
		r.cache.diskMu.Lock()
		defer r.cache.diskMu.Unlock()
		r.cache.diskReaders--
		if r.cache.diskClosed && r.cache.diskReaders == 0 {
			r.err = errors.Join(r.err, r.cache.removeDiskFile())
		}
	})
	return r.err
}

func (c *CachedFileData) SetBase64Data(data string) {
	c.diskMu.Lock()
	defer c.diskMu.Unlock()
	if !c.isDisk {
		c.base64Data = data
	}
}

func (c *CachedFileData) IsDisk() bool {
	return c.isDisk
}

func (c *CachedFileData) Close() error {
	c.diskMu.Lock()
	defer c.diskMu.Unlock()
	if !c.isDisk {
		c.base64Data = ""
		return nil
	}

	c.diskClosed = true
	if c.diskReaders != 0 {
		return nil
	}
	return c.removeDiskFile()
}

// removeDiskFile requires diskMu and no active readers. Keep the path on error
// so a later Close can retry cleanup without decrementing statistics twice.
func (c *CachedFileData) removeDiskFile() error {
	if c.diskPath != "" {
		err := os.Remove(c.diskPath)
		if os.IsNotExist(err) {
			// A background cleanup may already have removed and counted it.
			c.diskPath = ""
			return nil
		}
		if err == nil && !c.statDecremented && c.OnClose != nil {
			c.OnClose(c.DiskSize)
			c.statDecremented = true
		}
		if err == nil {
			c.diskPath = ""
		}
		return err
	}
	return nil
}
