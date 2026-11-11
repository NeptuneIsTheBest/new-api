package service

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"

	"github.com/gin-gonic/gin"
)

func CloseResponseBodyGracefully(httpResponse *http.Response) {
	if httpResponse == nil || httpResponse.Body == nil {
		return
	}
	err := httpResponse.Body.Close()
	if err != nil {
		common.SysError(common.LogText("failed to close response body: %s", err.Error()))
	}
}

// ShouldCopyUpstreamHeader checks whether a given upstream response header
// should be copied to the client response. It returns false for Content-Length
// (managed separately) and X-Oneapi-Request-Id (to preserve the local instance
// ID). When the upstream header is X-Oneapi-Request-Id, the value is captured
// into the Gin context for later logging.
func ShouldCopyUpstreamHeader(c *gin.Context, k string, v []string) bool {
	if strings.EqualFold(k, "Content-Length") {
		return false
	}
	if strings.EqualFold(k, common.RequestIdKey) {
		if c != nil && len(v) > 0 {
			c.Set(common.UpstreamRequestIdKey, v[0])
		}
		return false
	}
	return true
}

// CodexResponseHeaders selects response metadata understood by Codex, preserving
// header presence and all values. It returns an independent, canonicalized map
// for both SSE headers and Responses WebSocket metadata events.
func CodexResponseHeaders(c *gin.Context, headers http.Header) http.Header {
	hopByHop := make(map[string]bool)
	for name, values := range headers {
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for token := range strings.SplitSeq(value, ",") {
					hopByHop[strings.ToLower(strings.TrimSpace(token))] = true
				}
			}
		}
	}
	selected := make(http.Header)
	for name, values := range headers {
		if !ShouldCopyUpstreamHeader(c, name, values) || len(values) == 0 {
			continue
		}
		lower := strings.ToLower(name)
		if hopByHop[lower] {
			continue
		}
		allowed := false
		switch lower {
		case "openai-model", "x-models-etag", "x-reasoning-included", "x-codex-turn-state", "x-request-id", "retry-after",
			"x-codex-safety-buffering-enabled", "x-codex-safety-buffering-faster-model",
			"x-codex-credits-has-credits", "x-codex-credits-unlimited", "x-codex-credits-balance":
			allowed = true
		default:
			if limit, ok := strings.CutPrefix(lower, "x-"); ok {
				for _, suffix := range []string{
					"-primary-used-percent", "-primary-window-minutes", "-primary-reset-at",
					"-secondary-used-percent", "-secondary-window-minutes", "-secondary-reset-at", "-limit-name",
				} {
					if id, ok := strings.CutSuffix(limit, suffix); ok && id != "" {
						allowed = true
						break
					}
				}
			}
		}
		if allowed {
			name = http.CanonicalHeaderKey(name)
			selected[name] = append(selected[name], values...)
		}
	}
	return selected
}

func IOCopyBytesGracefully(c *gin.Context, src *http.Response, data []byte) {
	if c.Writer == nil {
		return
	}

	body := io.NopCloser(bytes.NewBuffer(data))

	// We shouldn't set the header before we parse the response body, because the parse part may fail.
	// And then we will have to send an error response, but in this case, the header has already been set.
	// So the httpClient will be confused by the response.
	// For example, Postman will report error, and we cannot check the response at all.
	if src != nil {
		for k, v := range src.Header {
			if !ShouldCopyUpstreamHeader(c, k, v) {
				continue
			}
			c.Writer.Header().Set(k, v[0])
		}
	}

	// set Content-Length header manually BEFORE calling WriteHeader
	c.Writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))

	// Write header with status code (this sends the headers)
	if src != nil {
		c.Writer.WriteHeader(src.StatusCode)
	} else {
		c.Writer.WriteHeader(http.StatusOK)
	}

	_, err := io.Copy(c.Writer, body)
	if err != nil {
		logger.LogError(c, common.LogText("failed to copy response body: %s", err.Error()))
	}
	c.Writer.Flush()
}
