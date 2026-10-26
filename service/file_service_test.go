package service

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configureFileCache(t *testing.T, thresholdMB int) {
	t.Helper()
	previous, previousLimit := common.GetDiskCacheConfig(), constant.MaxFileDownloadMB
	t.Cleanup(func() {
		common.SetDiskCacheConfig(previous)
		constant.MaxFileDownloadMB = previousLimit
	})
	common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: true, ThresholdMB: thresholdMB, MaxSizeMB: 16, Path: t.TempDir()})
	constant.MaxFileDownloadMB = 4
}

// Check that encoding has reached disk before the response has been consumed.
type fileDownloadReader struct {
	t       *testing.T
	reader  io.Reader
	read    int
	checked bool
}

func (r *fileDownloadReader) Read(p []byte) (int, error) {
	if r.read >= 1<<20 && !r.checked {
		count, size, err := common.GetDiskCacheInfo()
		require.NoError(r.t, err)
		require.Equal(r.t, 1, count)
		require.Positive(r.t, size, "the prefix must be encoded to disk before downloading the remaining bytes")
		r.checked = true
	}
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestFileDownloadStreamingCache(t *testing.T) {
	configureFileCache(t, 1)
	const rawThreshold = (1 << 20) / 4 * 3
	for _, tc := range []struct {
		name   string
		size   int
		length int64
		disk   bool
	}{
		{name: "empty", length: -1},
		{name: "one trailing byte", size: 1, length: 1},
		{name: "two trailing bytes", size: 2, length: -1},
		{name: "below encoded threshold", size: rawThreshold - 3, length: -1},
		{name: "at encoded threshold", size: rawThreshold, length: -1, disk: true},
		{name: "above encoded threshold", size: rawThreshold + 1, length: -1, disk: true},
		{name: "known large", size: 2 << 20, length: 2 << 20, disk: true},
		{name: "chunked large", size: 2 << 20, length: -1, disk: true},
		{name: "underestimated length", size: 2 << 20, length: 1, disk: true},
		{name: "overestimated length", size: 2 << 20, length: 3 << 20, disk: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := common.GetDiskCacheStats()
			payload := bytes.Repeat([]byte("a"), tc.size)
			reader := &fileDownloadReader{t: t, reader: bytes.NewReader(payload)}
			response := &http.Response{Body: io.NopCloser(reader), ContentLength: tc.length, Header: http.Header{"Content-Type": {"application/pdf"}}}
			data, err := cacheDownloadedFile(nil, response, "https://example.test/file")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, data.Close()) })
			assert.Equal(t, tc.disk, data.IsDisk())
			assert.EqualValues(t, tc.size, data.Size)
			assert.Equal(t, "application/pdf", data.MimeType)
			assert.Equal(t, tc.size > 1<<20, reader.checked)
			encoded, err := data.GetBase64Data()
			require.NoError(t, err)
			assert.Equal(t, base64.StdEncoding.EncodeToString(payload), encoded)
			decoded, err := data.OpenDecodedReader()
			require.NoError(t, err)
			actual, err := io.ReadAll(decoded)
			require.NoError(t, err)
			assert.Equal(t, payload, actual)
			require.NoError(t, decoded.Close())
			if tc.disk {
				assert.EqualValues(t, len(encoded), data.DiskSize)
				assert.Equal(t, before.CurrentDiskUsageBytes+data.DiskSize, common.GetDiskCacheStats().CurrentDiskUsageBytes)
			}
			require.NoError(t, data.Close())
			assert.Equal(t, before, common.GetDiskCacheStats())
		})
	}
}

func TestFileDownloadErrorsAndMemoryFallback(t *testing.T) {
	configureFileCache(t, 0)
	readErr := errors.New("download interrupted")
	for _, tc := range []struct {
		name, wantError string
		payload         io.Reader
		limit           int
	}{
		{name: "read before encoding", payload: iotest.ErrReader(readErr), limit: 4, wantError: "download interrupted"},
		{name: "read after spilling", payload: io.MultiReader(strings.NewReader(strings.Repeat("x", 8192)), iotest.ErrReader(readErr)), limit: 4, wantError: "download interrupted"},
		{name: "actual bytes exceed limit", payload: strings.NewReader(strings.Repeat("x", (1<<20)+1)), limit: 1, wantError: "file size exceeds maximum allowed size"},
		{name: "exact limit", payload: strings.NewReader(strings.Repeat("x", 1<<20)), limit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := common.GetDiskCacheStats()
			constant.MaxFileDownloadMB = tc.limit
			response := &http.Response{Body: io.NopCloser(tc.payload), ContentLength: 1, Header: http.Header{"Content-Type": {"application/pdf"}}}
			data, err := cacheDownloadedFile(nil, response, "https://example.test/file")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				assert.Nil(t, data)
			} else {
				require.NoError(t, err)
				require.NoError(t, data.Close())
			}
			assert.Equal(t, before, common.GetDiskCacheStats())
			count, _, err := common.GetDiskCacheInfo()
			require.NoError(t, err)
			assert.Zero(t, count)
		})
	}
	for _, mode := range []string{"disabled", "capacity unavailable", "creation failure"} {
		t.Run(mode, func(t *testing.T) {
			configureFileCache(t, 0)
			config := common.GetDiskCacheConfig()
			switch mode {
			case "disabled":
				config.Enabled = false
			case "capacity unavailable":
				config.MaxSizeMB = 0
			case "creation failure":
				config.Path = filepath.Join(t.TempDir(), "not-a-directory")
				require.NoError(t, os.WriteFile(config.Path, []byte("blocked"), 0600))
			}
			common.SetDiskCacheConfig(config)
			data, err := cacheDownloadedFile(nil, &http.Response{Body: io.NopCloser(strings.NewReader("payload")), Header: http.Header{}}, "https://example.test/file")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, data.Close()) })
			assert.False(t, data.IsDisk())
			encoded, err := data.GetBase64Data()
			require.NoError(t, err)
			assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("payload")), encoded)
		})
	}
}

func TestFileCacheWriteFailuresRemovePartialFiles(t *testing.T) {
	configureFileCache(t, 0)
	for _, failAt := range []string{"write", "encoder tail", "close", "capacity"} {
		t.Run(failAt, func(t *testing.T) {
			before := common.GetDiskCacheStats()
			writer := fileCacheWriter{}
			t.Cleanup(writer.abort)
			_, err := writer.Write([]byte("YWJj"))
			require.NoError(t, err)
			if failAt == "capacity" {
				config := common.GetDiskCacheConfig()
				common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: true})
				_, err = writer.Write([]byte("ZA=="))
				common.SetDiskCacheConfig(config)
			} else {
				require.NoError(t, writer.file.Close())
				switch failAt {
				case "write":
					_, err = writer.Write([]byte("ZA=="))
				case "encoder tail":
					encoder := base64.NewEncoder(base64.StdEncoding, &writer)
					_, err = encoder.Write([]byte("d"))
					require.NoError(t, err)
					err = encoder.Close()
				case "close":
					_, err = writer.finish("application/pdf", 3)
				}
			}
			require.Error(t, err)
			writer.abort()
			count, _, err := common.GetDiskCacheInfo()
			require.NoError(t, err)
			assert.Zero(t, count)
			assert.Equal(t, before, common.GetDiskCacheStats())
		})
	}
}

func TestCachedFileReadersAndCleanup(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk=%t", disk), func(t *testing.T) {
			configureFileCache(t, 0)
			config := common.GetDiskCacheConfig()
			config.Enabled = disk
			common.SetDiskCacheConfig(config)
			before := common.GetDiskCacheStats()
			const payload = "independent readers"
			encoded := base64.StdEncoding.EncodeToString([]byte(payload))
			data, err := loadFromBase64(encoded, "application/pdf")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, data.Close()) })
			first, err := data.OpenBase64Reader()
			require.NoError(t, err)
			defer first.Close()
			second, err := data.OpenDecodedReader()
			require.NoError(t, err)
			defer second.Close()
			prefix := make([]byte, 3)
			_, err = io.ReadFull(first, prefix)
			require.NoError(t, err)
			assert.Equal(t, encoded[:3], string(prefix))
			require.NoError(t, data.Close())
			require.NoError(t, data.Close())
			if disk {
				_, err := data.OpenBase64Reader()
				require.ErrorContains(t, err, "already closed")
				assert.Equal(t, before.ActiveDiskFiles+1, common.GetDiskCacheStats().ActiveDiskFiles)
			}
			decoded, err := io.ReadAll(second)
			require.NoError(t, err)
			assert.Equal(t, payload, string(decoded))
			require.NoError(t, second.Close())
			require.NoError(t, second.Close())
			rest, err := io.ReadAll(first)
			require.NoError(t, err)
			assert.Equal(t, encoded[3:], string(rest))
			require.NoError(t, first.Close())
			require.NoError(t, data.Close())
			assert.Equal(t, before, common.GetDiskCacheStats())
		})
	}
}

func TestFileBase64ValidationAndSharedCleanup(t *testing.T) {
	configureFileCache(t, 0)
	for _, input := range []string{
		"", "YQ==", "YWI=", "YWJj", "Y\r\nQ=\n=\r\n", "YQ", "YQ=", "!", "YQ==YWJj", "====",
		strings.Repeat("YWJj", 255) + "YQ==YWJj", // padding exactly at the decoder's block boundary
	} {
		_, decodeErr := base64.StdEncoding.DecodeString(input)
		data, err := loadFromBase64(input, "application/pdf")
		if decodeErr != nil {
			require.Error(t, err, input)
			assert.Nil(t, data)
			continue
		}
		require.NoError(t, err)
		assert.True(t, data.IsDisk(), "zero threshold also caches empty inputs on disk")
		actual, err := data.GetBase64Data()
		require.NoError(t, err)
		assert.Equal(t, input, actual)
		require.NoError(t, data.Close())
	}
	before := common.GetDiskCacheStats()
	c, _ := gin.CreateTestContext(nil)
	first := types.NewBase64FileSource("data:application/pdf;base64,YQ==", "")
	second := types.NewBase64FileSource("data:application/pdf;base64,YQ==", "")
	a, err := LoadFileSource(c, first)
	require.NoError(t, err)
	b, err := LoadFileSource(c, second)
	require.NoError(t, err)
	assert.Same(t, a, b)
	assert.Equal(t, "application/pdf", a.MimeType)
	assert.EqualValues(t, 1, a.Size)
	CleanupFileSources(c)
	assert.Equal(t, before, common.GetDiskCacheStats())
	path, err := common.WriteDiskCacheFileString(common.DiskCacheTypeFile, "cached string")
	require.NoError(t, err)
	defer os.Remove(path)
	text, err := common.ReadDiskCacheFileString(path)
	require.NoError(t, err)
	assert.Equal(t, "cached string", text)
}

func fileTestBox(kind string, content []byte, extended bool) []byte {
	header := 8
	if extended {
		header = 16
	}
	box := make([]byte, header+len(content))
	copy(box[4:8], kind)
	if extended {
		binary.BigEndian.PutUint32(box, 1)
		binary.BigEndian.PutUint64(box[8:], uint64(len(box)))
	} else {
		binary.BigEndian.PutUint32(box, uint32(len(box)))
	}
	copy(box[header:], content)
	return box
}

func TestFileImageMetadataFromStream(t *testing.T) {
	configureFileCache(t, 0)
	img := image.NewRGBA(image.Rect(0, 0, 7, 11))
	var pngData, jpegData, gifData bytes.Buffer
	require.NoError(t, png.Encode(&pngData, img))
	require.NoError(t, jpeg.Encode(&jpegData, img, nil))
	require.NoError(t, gif.Encode(&gifData, img, nil))
	// A WebP extended header carries the canvas dimensions independently of
	// the compressed image, which DecodeConfig deliberately does not read.
	webp := []byte("RIFF\x16\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00\x06\x00\x00\x0a\x00\x00")
	// Put the JPEG dimensions beyond the former fixed-size sniffing prefixes.
	jpegLate := append([]byte{}, jpegData.Bytes()[:2]...)
	for range 2 {
		jpegLate = append(jpegLate, 0xff, 0xe1, 0xff, 0xff)
		jpegLate = append(jpegLate, make([]byte, 65533)...)
	}
	jpegLate = append(jpegLate, jpegData.Bytes()[2:]...)
	ispe := make([]byte, 12)
	binary.BigEndian.PutUint32(ispe[4:], 7)
	binary.BigEndian.PutUint32(ispe[8:], 11)
	properties := fileTestBox("iprp", fileTestBox("ipco", fileTestBox("ispe", ispe, false), false), false)
	meta := fileTestBox("meta", append(make([]byte, 4), properties...), true)
	heif := fileTestBox("ftyp", []byte("heic\x00\x00\x00\x00"), false)
	heif = append(heif, fileTestBox("mdat", make([]byte, 1<<20), false)...)
	heif = append(heif, meta...)
	for _, tc := range []struct {
		name, format, mime string
		body               []byte
	}{
		{"PNG", "png", "image/png", pngData.Bytes()},
		{"GIF", "gif", "image/gif", gifData.Bytes()},
		{"WebP", "webp", "image/webp", webp},
		{"JPEG late dimensions", "jpeg", "image/jpeg", jpegLate},
		{"HEIC late extended meta", "heic", "image/heic", heif},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{Body: io.NopCloser(bytes.NewReader(tc.body)), ContentLength: -1, Header: http.Header{}}
			data, err := cacheDownloadedFile(nil, response, "https://example.test/file")
			require.NoError(t, err)
			defer data.Close()
			require.NotNil(t, data.ImageConfig)
			assert.Equal(t, 7, data.ImageConfig.Width)
			assert.Equal(t, 11, data.ImageConfig.Height)
			assert.Equal(t, tc.mime, data.MimeType)
			assert.Equal(t, tc.format, data.ImageFormat)
			// Exercise lazy metadata reading from the disk cache as well.
			data.ImageConfig = nil
			source := types.NewURLFileSource("https://example.test/file")
			source.SetCache(data)
			config, format, err := GetImageConfig(nil, source)
			require.NoError(t, err)
			assert.Equal(t, 7, config.Width)
			assert.Equal(t, 11, config.Height)
			assert.Equal(t, tc.format, format)
			inline, err := loadFromBase64(base64.StdEncoding.EncodeToString(tc.body), "")
			require.NoError(t, err)
			defer inline.Close()
			require.NotNil(t, inline.ImageConfig)
			assert.Equal(t, config.Width, inline.ImageConfig.Width)
			assert.Equal(t, config.Height, inline.ImageConfig.Height)
			assert.Equal(t, format, inline.ImageFormat)
		})
	}
	for _, invalid := range [][]byte{heif[:12], heif[:len(heif)-1], append(fileTestBox("ftyp", []byte("heic"), false), meta[:12]...)} {
		data := types.NewMemoryCachedData(base64.StdEncoding.EncodeToString(invalid), "image/heic", int64(len(invalid)))
		_, _, err := readCachedImageConfig(data)
		require.Error(t, err)
	}
	for _, tc := range []struct{ contentType, disposition, url, want string }{
		{"application/pdf; charset=utf-8", "", "https://example.test/file.png", "application/pdf"},
		{"application/octet-stream", `attachment; filename="file.pdf"`, "https://example.test/file.png", "application/pdf"},
		{"application/octet-stream", "", "https://example.test/file.jpg", "image/jpeg"},
	} {
		response := &http.Response{Header: http.Header{"Content-Type": {tc.contentType}, "Content-Disposition": {tc.disposition}}}
		assert.Equal(t, tc.want, smartDetectMimeType(response, tc.url, pngData.Bytes()))
	}
}
