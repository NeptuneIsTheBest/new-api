package relay

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gorilla/websocket"
)

type responsesWSMetadataEvent struct {
	Type     string            `json:"type"`
	Headers  map[string]string `json:"headers"`
	StreamID string            `json:"stream_id,omitempty"`
}

// sendHandshakeMetadata bridges headers that arrive after the client upgrade
// using events consumed by Codex ac54a051. X-Reasoning-Included is retained in
// the metadata but Codex only interprets that flag in the HTTP handshake.
func (s *responsesWSSession) sendHandshakeMetadata(headers map[string]string) error {
	if len(headers) == 0 {
		return nil
	}
	if turnState, present := headers["x-codex-turn-state"]; present {
		// Older chained gateways treat every response.* event as acceptance.
		// Retain even an empty turn state until an actual generation begins.
		s.stateMu.Lock()
		if !s.closing {
			s.pendingTurnState = &turnState
		}
		s.stateMu.Unlock()
	}
	events := []any{responsesWSMetadataEvent{Type: "codex.response.metadata", Headers: headers}}
	for _, event := range codexRateLimitEvents(headers) {
		events = append(events, event)
	}
	for _, event := range events {
		payload, err := common.Marshal(event)
		if err != nil {
			return err
		}
		if err := s.writeClient(websocket.TextMessage, payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *responsesWSSession) sendPendingTurnState(streamID string) error {
	s.stateMu.Lock()
	turnState := s.pendingTurnState
	s.pendingTurnState = nil
	s.stateMu.Unlock()
	if turnState == nil {
		return nil
	}
	// Associate this connection metadata with the accepted generation so an
	// older gateway cannot charge a different, still unaccepted lane for it.
	payload, err := common.Marshal(responsesWSMetadataEvent{
		Type: "response.metadata", Headers: map[string]string{"x-codex-turn-state": *turnState}, StreamID: streamID,
	})
	if err != nil {
		return err
	}
	return s.writeClient(websocket.TextMessage, payload)
}

type codexRateLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes *int64  `json:"window_minutes,omitempty"`
	ResetAt       *int64  `json:"reset_at,omitempty"`
}

type codexRateLimitDetails struct {
	Primary   *codexRateLimitWindow `json:"primary,omitempty"`
	Secondary *codexRateLimitWindow `json:"secondary,omitempty"`
}

type codexCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

type codexRateLimitEvent struct {
	Type             string                 `json:"type"`
	MeteredLimitName string                 `json:"metered_limit_name"`
	RateLimits       *codexRateLimitDetails `json:"rate_limits,omitempty"`
	Credits          *codexCredits          `json:"credits,omitempty"`
}

// codexRateLimitEvents follows codex-api/src/rate_limits.rs at ac54a051:
// discover extra families by their primary-used-percent header, share credits,
// and omit windows with no usable data. These are account metadata, not usage.
func codexRateLimitEvents(headers map[string]string) []codexRateLimitEvent {
	limits := []string{"codex"}
	for name := range headers {
		prefix, ok := strings.CutSuffix(name, "-primary-used-percent")
		if !ok {
			continue
		}
		limit, ok := strings.CutPrefix(prefix, "x-")
		limit = strings.ReplaceAll(limit, "-", "_")
		if ok && limit != "" && !slices.Contains(limits, limit) {
			limits = append(limits, limit)
		}
	}
	slices.Sort(limits[1:])

	var credits *codexCredits
	hasCredits := strings.ToLower(headers["x-codex-credits-has-credits"])
	unlimited := strings.ToLower(headers["x-codex-credits-unlimited"])
	if (hasCredits == "true" || hasCredits == "false" || hasCredits == "1" || hasCredits == "0") &&
		(unlimited == "true" || unlimited == "false" || unlimited == "1" || unlimited == "0") {
		credits = &codexCredits{
			HasCredits: hasCredits == "true" || hasCredits == "1",
			Unlimited:  unlimited == "true" || unlimited == "1",
			Balance:    strings.TrimSpace(headers["x-codex-credits-balance"]),
		}
	}

	var events []codexRateLimitEvent
	for _, limit := range limits {
		// The Codex parser normalizes underscores to dashes in header names.
		prefix := "x-" + strings.ReplaceAll(limit, "_", "-")
		primary := parseCodexRateLimitWindow(headers, prefix+"-primary")
		secondary := parseCodexRateLimitWindow(headers, prefix+"-secondary")
		if primary == nil && secondary == nil && credits == nil {
			continue
		}
		event := codexRateLimitEvent{Type: "codex.rate_limits", MeteredLimitName: limit, Credits: credits}
		if primary != nil || secondary != nil {
			event.RateLimits = &codexRateLimitDetails{Primary: primary, Secondary: secondary}
		}
		events = append(events, event)
	}
	return events
}

func parseCodexRateLimitWindow(headers map[string]string, prefix string) *codexRateLimitWindow {
	raw := headers[prefix+"-used-percent"]
	// Rust's float parser does not accept Go's hexadecimal or underscored
	// number literals. Non-finite values cannot be represented in JSON either.
	if strings.ContainsAny(raw, "xX_") {
		return nil
	}
	used, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) {
		return nil
	}
	window := &codexRateLimitWindow{UsedPercent: used}
	if minutes, err := strconv.ParseInt(headers[prefix+"-window-minutes"], 10, 64); err == nil {
		window.WindowMinutes = &minutes
	}
	if resetAt, err := strconv.ParseInt(headers[prefix+"-reset-at"], 10, 64); err == nil {
		window.ResetAt = &resetAt
	}
	if used == 0 && (window.WindowMinutes == nil || *window.WindowMinutes == 0) && window.ResetAt == nil {
		return nil
	}
	return window
}
