package service

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// fileCacheWriter keeps the encoded prefix in memory until it switches to disk.
// It batches small encoder writes for downloads and owns the temporary file
// until finish transfers it to CachedFileData.
type fileCacheWriter struct {
	context          *gin.Context
	buffer           strings.Builder
	file             *os.File
	diskBuffer       *bufio.Writer
	path             string
	size             int64
	expectedSize     int64
	memoryOnly       bool
	bufferDiskWrites bool
}

func (w *fileCacheWriter) Write(p []byte) (int, error) {
	nextSize := w.size + int64(len(p))
	if w.file == nil && !w.memoryOnly && common.ShouldUseDiskCache(max(nextSize, w.expectedSize)) {
		path, file, err := common.CreateDiskCacheFile(common.DiskCacheTypeFile)
		if err != nil {
			// No bytes have been written to disk; the prefix is still intact.
			w.memoryOnly = true
			var logContext context.Context = context.Background()
			if w.context != nil {
				logContext = w.context
			}
			logger.LogWarn(logContext, common.LogText("Failed to write to disk cache, falling back to memory: %v", err))
		} else {
			w.path, w.file = path, file
			if _, err := io.WriteString(file, w.buffer.String()); err != nil {
				return 0, fmt.Errorf("failed to write cache prefix: %w", err)
			}
			w.buffer.Reset()
			if w.bufferDiskWrites {
				w.diskBuffer = bufio.NewWriterSize(file, 32<<10)
			}
		}
	}
	if w.file == nil {
		n, err := w.buffer.Write(p)
		w.size += int64(n)
		return n, err
	}
	// Content-Length is only a hint: the actual encoded stream can be larger.
	if !common.IsDiskCacheAvailable(nextSize) {
		return 0, fmt.Errorf("disk cache capacity exceeded")
	}
	var dst io.Writer = w.file
	if w.diskBuffer != nil {
		dst = w.diskBuffer
	}
	n, err := dst.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *fileCacheWriter) finish(mimeType string, size int64) (*types.CachedFileData, error) {
	if w.size == 0 {
		// Preserve the zero-threshold policy for empty cached files too.
		if _, err := w.Write(nil); err != nil {
			return nil, err
		}
	}
	if w.file == nil {
		return types.NewMemoryCachedData(w.buffer.String(), mimeType, size), nil
	}
	if w.diskBuffer != nil {
		if err := w.diskBuffer.Flush(); err != nil {
			return nil, fmt.Errorf("failed to flush cache file: %w", err)
		}
	}
	if err := w.file.Close(); err != nil {
		return nil, fmt.Errorf("failed to close cache file: %w", err)
	}
	w.file = nil
	data := types.NewDiskCachedData(w.path, mimeType, size)
	data.DiskSize = w.size
	data.OnClose = common.DecrementDiskFiles
	common.IncrementDiskFiles(w.size)
	w.path = ""
	return data, nil
}

func (w *fileCacheWriter) abort() {
	if w.file != nil {
		_ = w.file.Close()
	}
	if w.path != "" {
		_ = os.Remove(w.path)
	}
}
