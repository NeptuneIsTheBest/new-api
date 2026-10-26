package service

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	_ "golang.org/x/image/webp"
)

// FileService 统一的文件处理服务
// 提供文件下载、解码、缓存等功能的统一入口

// getContextCacheKey 生成 URL context 缓存的 key
func getContextCacheKey(url string) string {
	return fmt.Sprintf("file_cache_%s", common.GenerateHMAC(url))
}

// getBase64ContextCacheKey 生成 base64 context 缓存的 key
// 使用 length + MIME + 完整 base64 数据，避免不同文件因前缀相同而共享缓存
func getBase64ContextCacheKey(data string, mimeType string) string {
	h := hmac.New(sha256.New, []byte(common.CryptoSecret))
	_, _ = fmt.Fprintf(h, "%d:%s:", len(data), mimeType)
	// Hide strings.Reader.WriteTo to avoid copying the entire string to []byte.
	_, _ = io.Copy(h, struct{ io.Reader }{strings.NewReader(data)})
	return fmt.Sprintf("b64_cache_%x", h.Sum(nil))
}

// LoadFileSource 加载文件源数据
// 这是统一的入口，会自动处理缓存和不同的来源类型
func LoadFileSource(c *gin.Context, source types.FileSource, reason ...string) (*types.CachedFileData, error) {
	if source == nil {
		return nil, fmt.Errorf("file source is nil")
	}

	if common.DebugEnabled {
		logger.LogDebug(c, "LoadFileSource starting for: %s", source.GetIdentifier())
	}

	// 1. 快速检查内部缓存
	if source.HasCache() {
		if c != nil {
			registerSourceForCleanup(c, source)
		}
		return source.GetCache(), nil
	}

	// 2. 加锁保护加载过程
	source.Mu().Lock()
	defer source.Mu().Unlock()

	// 3. 双重检查
	if source.HasCache() {
		if c != nil {
			registerSourceForCleanup(c, source)
		}
		return source.GetCache(), nil
	}

	// 4. 根据来源类型加载（含 URL context 缓存查找）
	var cachedData *types.CachedFileData
	var contextKey string
	var err error

	switch s := source.(type) {
	case *types.URLSource:
		if c != nil {
			contextKey = getContextCacheKey(s.URL)
			if cached, exists := c.Get(contextKey); exists {
				data := cached.(*types.CachedFileData)
				source.SetCache(data)
				registerSourceForCleanup(c, source)
				return data, nil
			}
		}
		cachedData, err = loadFromURL(c, s.URL, reason...)
	case *types.Base64Source:
		if c != nil {
			contextKey = getBase64ContextCacheKey(s.Base64Data, s.MimeType)
			if cached, exists := c.Get(contextKey); exists {
				data := cached.(*types.CachedFileData)
				source.SetCache(data)
				registerSourceForCleanup(c, source)
				return data, nil
			}
		}
		cachedData, err = loadFromBase64(s.Base64Data, s.MimeType)
	default:
		return nil, fmt.Errorf("unsupported file source type: %T", source)
	}

	if err != nil {
		return nil, err
	}

	// 5. 设置缓存
	source.SetCache(cachedData)
	if contextKey != "" && c != nil {
		c.Set(contextKey, cachedData)
	}

	// 6. 注册到 context 以便请求结束时自动清理
	if c != nil {
		registerSourceForCleanup(c, source)
	}

	return cachedData, nil
}

// registerSourceForCleanup 注册 FileSource 到 context 以便请求结束时清理
func registerSourceForCleanup(c *gin.Context, source types.FileSource) {
	if source.IsRegistered() {
		return
	}

	key := string(constant.ContextKeyFileSourcesToCleanup)
	var sources []types.FileSource
	if existing, exists := c.Get(key); exists {
		sources = existing.([]types.FileSource)
	}
	sources = append(sources, source)
	c.Set(key, sources)
	source.SetRegistered(true)
}

// CleanupFileSources 清理请求中所有注册的 FileSource
// 应在请求结束时调用（通常由中间件自动调用）
func CleanupFileSources(c *gin.Context) {
	key := string(constant.ContextKeyFileSourcesToCleanup)
	if sources, exists := c.Get(key); exists {
		for _, source := range sources.([]types.FileSource) {
			if cache := source.GetCache(); cache != nil {
				cache.Close()
			}
		}
		c.Set(key, nil)
	}
}

// loadFromURL 从 URL 加载文件
func loadFromURL(c *gin.Context, url string, reason ...string) (*types.CachedFileData, error) {
	resp, err := DoDownloadRequest(url, reason...)
	if err != nil {
		return nil, fmt.Errorf("failed to download file from %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download file, status code: %d", resp.StatusCode)
	}
	return cacheDownloadedFile(c, resp, url)
}

// cacheDownloadedFile limits the original bytes while encoding directly into
// the cache. Response length is only an allocation hint, never a size check.
func cacheDownloadedFile(c *gin.Context, resp *http.Response, url string) (*types.CachedFileData, error) {
	maxBytes := int64(constant.MaxFileDownloadMB) << 20
	reader := bufio.NewReader(io.LimitReader(resp.Body, maxBytes+1))
	prefix, err := reader.Peek(512)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to read file content: %w", err)
	}
	mimeType := smartDetectMimeType(resp, url, prefix)
	writer := fileCacheWriter{context: c}
	if resp.ContentLength > 0 {
		writer.expectedSize = int64(base64.StdEncoding.EncodedLen(int(min(resp.ContentLength, maxBytes))))
	}
	defer writer.abort()
	encoder := base64.NewEncoder(base64.StdEncoding, &writer)
	size, err := io.Copy(encoder, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to cache file content: %w", err)
	}
	if size > maxBytes {
		return nil, fmt.Errorf("file size exceeds maximum allowed size: %dMB", constant.MaxFileDownloadMB)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("failed to encode file content: %w", err)
	}
	cachedData, err := writer.finish(mimeType, size)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(mimeType, "image/") || mimeType == "application/octet-stream" || mimeType == "" {
		if config, format, err := readCachedImageConfig(cachedData); err == nil {
			cachedData.ImageConfig = &config
			cachedData.ImageFormat = format
			if mimeType == "application/octet-stream" || mimeType == "" {
				cachedData.MimeType = "image/" + format
			}
		}
	}
	return cachedData, nil
}

// smartDetectMimeType 智能检测 MIME 类型
func smartDetectMimeType(resp *http.Response, url string, fileBytes []byte) string {
	// 1. 尝试从 Content-Type header 获取
	mimeType := resp.Header.Get("Content-Type")
	if idx := strings.Index(mimeType, ";"); idx != -1 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	if mimeType != "" && mimeType != "application/octet-stream" {
		return mimeType
	}

	// 2. 尝试从 Content-Disposition header 的 filename 获取
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		parts := strings.SplitSeq(cd, ";")
		for part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(strings.ToLower(part), "filename=") {
				name := strings.TrimSpace(strings.TrimPrefix(part, "filename="))
				// 移除引号
				if len(name) > 2 && name[0] == '"' && name[len(name)-1] == '"' {
					name = name[1 : len(name)-1]
				}
				if dot := strings.LastIndex(name, "."); dot != -1 && dot+1 < len(name) {
					ext := strings.ToLower(name[dot+1:])
					if ext != "" {
						mt := GetMimeTypeByExtension(ext)
						if mt != "application/octet-stream" {
							return mt
						}
					}
				}
				break
			}
		}
	}

	// 3. 尝试从 URL 路径获取扩展名
	mt := guessMimeTypeFromURL(url)
	if mt != "application/octet-stream" {
		return mt
	}

	// 4. 使用 http.DetectContentType 内容嗅探
	if len(fileBytes) > 0 {
		sniffed := http.DetectContentType(fileBytes)
		if sniffed != "" && sniffed != "application/octet-stream" {
			// 去除可能的 charset 参数
			if idx := strings.Index(sniffed, ";"); idx != -1 {
				sniffed = strings.TrimSpace(sniffed[:idx])
			}
			return sniffed
		}

		// 4.5 尝试 HEIF/HEIC 检测（Go 标准库不识别）
		if heifMime := detectHEIF(fileBytes); heifMime != "" {
			return heifMime
		}
	}

	// 最终回退
	return "application/octet-stream"
}

// loadFromBase64 从 base64 字符串加载文件
func loadFromBase64(base64String string, providedMimeType string) (*types.CachedFileData, error) {
	var mimeType string
	var cleanBase64 string

	// 处理 data: 前缀
	if strings.HasPrefix(base64String, "data:") {
		before, after, ok := strings.Cut(base64String, ",")
		if ok {
			header := before
			cleanBase64 = after

			if strings.Contains(header, ":") && strings.Contains(header, ";") {
				mimeStart := strings.Index(header, ":") + 1
				mimeEnd := strings.Index(header, ";")
				if mimeStart < mimeEnd {
					mimeType = header[mimeStart:mimeEnd]
				}
			}
		} else {
			cleanBase64 = base64String
		}
	} else {
		cleanBase64 = base64String
	}

	if providedMimeType != "" {
		mimeType = providedMimeType
	}

	cachedData := types.NewMemoryCachedData(cleanBase64, mimeType, 0)
	decoded, err := cachedData.OpenDecodedReader()
	if err != nil {
		return nil, err
	}
	size, err := io.Copy(io.Discard, decoded)
	_ = decoded.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64 data: %w", err)
	}
	base64Size := int64(len(cleanBase64))
	cachedData.Size = size
	if common.ShouldUseDiskCache(base64Size) {
		writer := fileCacheWriter{expectedSize: base64Size}
		defer writer.abort()
		// Hide strings.Reader.WriteTo: its fallback for a Writer without
		// WriteString would convert the entire string to []byte at once.
		if _, err := io.Copy(&writer, struct{ io.Reader }{strings.NewReader(cleanBase64)}); err != nil {
			return nil, fmt.Errorf("failed to cache base64 data: %w", err)
		}
		cachedData, err = writer.finish(mimeType, size)
		if err != nil {
			return nil, err
		}
	}
	if mimeType == "" || strings.HasPrefix(mimeType, "image/") {
		if config, format, err := readCachedImageConfig(cachedData); err == nil {
			cachedData.ImageConfig = &config
			cachedData.ImageFormat = format
			if mimeType == "" {
				cachedData.MimeType = "image/" + format
			}
		}
	}

	return cachedData, nil
}

// GetImageConfig 获取图片配置
func GetImageConfig(c *gin.Context, source types.FileSource) (image.Config, string, error) {
	cachedData, err := LoadFileSource(c, source, "get_image_config")
	if err != nil {
		return image.Config{}, "", err
	}

	if cachedData.ImageConfig != nil {
		return *cachedData.ImageConfig, cachedData.ImageFormat, nil
	}

	config, format, err := readCachedImageConfig(cachedData)
	if err != nil {
		return image.Config{}, "", err
	}

	cachedData.ImageConfig = &config
	cachedData.ImageFormat = format

	return config, format, nil
}

// GetBase64Data 获取 base64 编码的数据
func GetBase64Data(c *gin.Context, source types.FileSource, reason ...string) (string, string, error) {
	cachedData, err := LoadFileSource(c, source, reason...)
	if err != nil {
		return "", "", err
	}
	base64Str, err := cachedData.GetBase64Data()
	if err != nil {
		return "", "", fmt.Errorf("failed to get base64 data: %w", err)
	}
	return base64Str, cachedData.MimeType, nil
}

// GetMimeType 获取文件的 MIME 类型
func GetMimeType(c *gin.Context, source types.FileSource) (string, error) {
	if source.HasCache() {
		return source.GetCache().MimeType, nil
	}

	if urlSource, ok := source.(*types.URLSource); ok {
		mimeType, err := GetFileTypeFromUrl(c, urlSource.URL, "get_mime_type")
		if err == nil && mimeType != "" && mimeType != "application/octet-stream" {
			return mimeType, nil
		}
	}

	cachedData, err := LoadFileSource(c, source, "get_mime_type")
	if err != nil {
		return "", err
	}
	return cachedData.MimeType, nil
}

// DetectFileType 检测文件类型
func DetectFileType(mimeType string) types.FileType {
	if strings.HasPrefix(mimeType, "image/") {
		return types.FileTypeImage
	}
	if strings.HasPrefix(mimeType, "audio/") {
		return types.FileTypeAudio
	}
	if strings.HasPrefix(mimeType, "video/") {
		return types.FileTypeVideo
	}
	return types.FileTypeFile
}

// readCachedImageConfig reads only the image headers needed by the decoder.
func readCachedImageConfig(data *types.CachedFileData) (image.Config, string, error) {
	reader, err := data.OpenDecodedReader()
	if err != nil {
		return image.Config{}, "", err
	}
	defer reader.Close()
	buffered := bufio.NewReader(reader)
	prefix, _ := buffered.Peek(12)
	if heifMime := detectHEIF(prefix); heifMime != "" {
		if w, h, ok := readHEIFDimensions(buffered, data.Size, 0); ok {
			return image.Config{Width: w, Height: h}, strings.TrimPrefix(heifMime, "image/"), nil
		}
		return image.Config{}, "", fmt.Errorf("failed to decode HEIF/HEIC image dimensions")
	}
	return image.DecodeConfig(buffered)
}

// detectHEIF checks ISOBMFF magic bytes to detect HEIC/HEIF files.
// Returns "image/heic", "image/heif", or "" if not recognized.
func detectHEIF(data []byte) string {
	if len(data) < 12 {
		return ""
	}
	// ISOBMFF: bytes[4:8] must be "ftyp"
	if string(data[4:8]) != "ftyp" {
		return ""
	}
	brand := string(data[8:12])
	switch brand {
	case "heic", "heix", "hevc", "hevx", "heim", "heis":
		return "image/heic"
	case "mif1", "msf1":
		return "image/heif"
	default:
		return ""
	}
}

// parseHEIFDimensions parses ISOBMFF box tree to find the ispe box
// and extract image width/height. Returns (width, height, ok).
func parseHEIFDimensions(data []byte) (int, int, bool) {
	return readHEIFDimensions(bytes.NewReader(data), int64(len(data)), 0)
}

// readHEIFDimensions walks bounded ISOBMFF containers without retaining their
// payloads. The original byte-slice entry point also uses this parser.
func readHEIFDimensions(reader io.Reader, size int64, depth int) (int, int, bool) {
	if depth > 64 {
		return 0, 0, false
	}
	var header [16]byte
	for size >= 8 {
		if _, err := io.ReadFull(reader, header[:8]); err != nil {
			return 0, 0, false
		}
		boxSize := uint64(binary.BigEndian.Uint32(header[:4]))
		boxType := string(header[4:8])
		headerSize := uint64(8)
		if boxSize == 1 {
			if size < 16 {
				return 0, 0, false
			}
			if _, err := io.ReadFull(reader, header[8:16]); err != nil {
				return 0, 0, false
			}
			boxSize = binary.BigEndian.Uint64(header[8:16])
			headerSize = 16
		} else if boxSize == 0 {
			boxSize = uint64(size)
		}
		if boxSize < headerSize || boxSize > uint64(size) {
			return 0, 0, false
		}
		content := &io.LimitedReader{R: reader, N: int64(boxSize - headerSize)}
		if depth == 0 && boxType == "meta" {
			if _, err := io.ReadFull(content, header[:4]); err != nil {
				return 0, 0, false
			}
			return readHEIFDimensions(content, content.N, depth+1)
		}
		if depth > 0 {
			switch boxType {
			case "iprp", "ipco":
				if w, h, ok := readHEIFDimensions(content, content.N, depth+1); ok {
					return w, h, true
				}
			case "ispe":
				if _, err := io.ReadFull(content, header[:12]); err == nil {
					w := int(binary.BigEndian.Uint32(header[4:8]))
					h := int(binary.BigEndian.Uint32(header[8:12]))
					if w > 0 && h > 0 {
						return w, h, true
					}
				}
			}
		}
		if _, err := io.Copy(io.Discard, content); err != nil || content.N != 0 {
			return 0, 0, false
		}
		size -= int64(boxSize)
	}
	return 0, 0, false
}

// guessMimeTypeFromURL 从 URL 猜测 MIME 类型
func guessMimeTypeFromURL(url string) string {
	cleanedURL := url
	if q := strings.Index(cleanedURL, "?"); q != -1 {
		cleanedURL = cleanedURL[:q]
	}

	if slash := strings.LastIndex(cleanedURL, "/"); slash != -1 && slash+1 < len(cleanedURL) {
		last := cleanedURL[slash+1:]
		if dot := strings.LastIndex(last, "."); dot != -1 && dot+1 < len(last) {
			ext := strings.ToLower(last[dot+1:])
			return GetMimeTypeByExtension(ext)
		}
	}

	return "application/octet-stream"
}
