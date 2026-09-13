package middleware

import (
	"bufio"
	"errors"
	"io"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

var errInvalidModelJSON = errors.New("invalid JSON request body")

// modelJSONScanner validates the whole document but retains only the first
// top-level model and group strings. Decoder.Decode buffers an entire value,
// and Decoder.Token still allocates entire strings, including base64 payloads.
type modelJSONScanner struct {
	reader     *bufio.Reader
	fields     [2]gjson.Result
	modelCount int
	groupSeen  bool
}

func readModelRequestJSON(reader io.Reader) (*ModelRequest, error) {
	scanner := modelJSONScanner{reader: bufio.NewReaderSize(reader, 32<<10)}
	first, err := scanner.nonSpace()
	if err != nil {
		return nil, err
	}
	if _, err := scanner.value(first, 0, false); err != nil {
		return nil, err
	}
	// Do not stop at the model or closing delimiter: trailing data is invalid.
	for {
		current, err := scanner.reader.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch current {
		case ' ', '\t', '\r', '\n':
		default:
			return nil, errInvalidModelJSON
		}
	}
	if scanner.modelCount > 1 {
		return nil, errors.New("model must be provided once")
	}
	model, err := getJSONStringValue(scanner.fields[0], "model")
	if err != nil {
		return nil, err
	}
	group, err := getJSONStringValue(scanner.fields[1], "group")
	if err != nil {
		return nil, err
	}
	return &ModelRequest{Model: model, Group: group}, nil
}

func (s *modelJSONScanner) readByte() (byte, error) {
	current, err := s.reader.ReadByte()
	if err == io.EOF {
		return 0, errInvalidModelJSON
	}
	return current, err
}

func (s *modelJSONScanner) nonSpace() (byte, error) {
	for {
		current, err := s.readByte()
		if err != nil {
			return 0, err
		}
		switch current {
		case ' ', '\t', '\r', '\n':
		default:
			return current, nil
		}
	}
}

func (s *modelJSONScanner) value(first byte, depth int, capture bool) (gjson.Result, error) {
	switch first {
	case '{', '[':
		// Match the nesting limit of the host's JSON decoder.
		if depth >= 10000 {
			return gjson.Result{}, errInvalidModelJSON
		}
		var err error
		if first == '{' {
			err = s.object(depth + 1)
		} else {
			err = s.array(depth + 1)
		}
		return gjson.Result{Type: gjson.JSON}, err
	case '"':
		limit := 0
		if capture {
			limit = -1
		}
		raw, err := s.stringValue(limit)
		if err != nil || !capture {
			return gjson.Result{}, err
		}
		return gjson.ParseBytes(raw), nil
	case 'n':
		return gjson.Result{}, s.literal("ull")
	case 't':
		return gjson.Result{Type: gjson.True}, s.literal("rue")
	case 'f':
		return gjson.Result{Type: gjson.False}, s.literal("alse")
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return gjson.Result{Type: gjson.Number}, s.number(first)
	default:
		return gjson.Result{}, errInvalidModelJSON
	}
}

func (s *modelJSONScanner) object(depth int) error {
	current, err := s.nonSpace()
	if err != nil || current == '}' {
		return err
	}
	for {
		if current != '"' {
			return errInvalidModelJSON
		}
		keyLimit := 0
		if depth == 1 {
			// Both names have five ASCII characters. Even fully escaped, a
			// matching key needs at most 5*6 bytes plus its surrounding quotes.
			keyLimit = 32
		}
		raw, err := s.stringValue(keyLimit)
		if err != nil {
			return err
		}
		var key string
		if raw != nil {
			if err := common.Unmarshal(raw, &key); err != nil {
				return err
			}
		}
		current, err = s.nonSpace()
		if err != nil {
			return err
		}
		if current != ':' {
			return errInvalidModelJSON
		}
		first, err := s.nonSpace()
		if err != nil {
			return err
		}
		field := -1
		switch key {
		case "model":
			s.modelCount = min(s.modelCount+1, 2)
			if s.modelCount == 1 {
				field = 0
			}
		case "group":
			if !s.groupSeen {
				field = 1
				s.groupSeen = true
			}
		}
		value, err := s.value(first, depth, field >= 0)
		if err != nil {
			return err
		}
		if field >= 0 {
			s.fields[field] = value
		}
		current, err = s.nonSpace()
		if err != nil || current == '}' {
			return err
		}
		if current != ',' {
			return errInvalidModelJSON
		}
		current, err = s.nonSpace()
		if err != nil {
			return err
		}
	}
}

func (s *modelJSONScanner) array(depth int) error {
	current, err := s.nonSpace()
	if err != nil || current == ']' {
		return err
	}
	for {
		if _, err := s.value(current, depth, false); err != nil {
			return err
		}
		current, err = s.nonSpace()
		if err != nil || current == ']' {
			return err
		}
		if current != ',' {
			return errInvalidModelJSON
		}
		current, err = s.nonSpace()
		if err != nil {
			return err
		}
	}
}

// stringValue consumes a string after its opening quote. A negative limit
// captures it all, zero discards it, and a positive limit discards overlong keys.
// Ignored strings are never decoded or accumulated, regardless of their size.
func (s *modelJSONScanner) stringValue(limit int) ([]byte, error) {
	var raw []byte
	if limit != 0 {
		raw = append(raw, '"')
	}
	escaped := false
	hexRemaining := 0
	for {
		current, err := s.readByte()
		if err != nil {
			return nil, err
		}
		if limit != 0 {
			if limit > 0 && len(raw) == limit {
				raw = nil
				limit = 0
			} else {
				raw = append(raw, current)
			}
		}
		if hexRemaining > 0 {
			if !(current >= '0' && current <= '9' || current >= 'a' && current <= 'f' || current >= 'A' && current <= 'F') {
				return nil, errInvalidModelJSON
			}
			hexRemaining--
			continue
		}
		if escaped {
			escaped = false
			switch current {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				hexRemaining = 4
			default:
				return nil, errInvalidModelJSON
			}
			continue
		}
		switch {
		case current == '"':
			return raw, nil
		case current == '\\':
			escaped = true
		case current < ' ':
			return nil, errInvalidModelJSON
		}
	}
}

func (s *modelJSONScanner) literal(remaining string) error {
	for i := range len(remaining) {
		current, err := s.readByte()
		if err != nil {
			return err
		}
		if current != remaining[i] {
			return errInvalidModelJSON
		}
	}
	return nil
}

func (s *modelJSONScanner) number(first byte) error {
	current := first
	if current == '-' {
		var err error
		current, err = s.readByte()
		if err != nil {
			return err
		}
	}
	if current < '0' || current > '9' {
		return errInvalidModelJSON
	}
	// Track number grammar without buffering its digits or converting it to a
	// float. Even numbers outside float64's range are valid in ignored fields.
	leadingZero := current == '0'
	fraction := false
	exponent := false
	needDigit := false
	allowSign := false
	for {
		peek, err := s.reader.Peek(1)
		if err != nil && err != io.EOF {
			return err
		}
		if err == io.EOF {
			if needDigit {
				return errInvalidModelJSON
			}
			return nil
		}
		current = peek[0]
		switch {
		case current >= '0' && current <= '9':
			if leadingZero && !fraction && !exponent {
				return errInvalidModelJSON
			}
			needDigit = false
			allowSign = false
		case current == '.' && !fraction && !exponent && !needDigit:
			fraction = true
			needDigit = true
		case (current == 'e' || current == 'E') && !exponent && !needDigit:
			exponent = true
			needDigit = true
			allowSign = true
		case (current == '+' || current == '-') && allowSign:
			allowSign = false
		default:
			if needDigit {
				return errInvalidModelJSON
			}
			return nil
		}
		if _, err := s.reader.Discard(1); err != nil {
			return err
		}
	}
}
