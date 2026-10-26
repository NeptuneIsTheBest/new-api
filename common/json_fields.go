package common

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// ErrInvalidJSON indicates invalid syntax or more than one JSON document.
var ErrInvalidJSON = errors.New("invalid JSON request body")

// JSONStringField describes the first occurrence of a selected top-level key.
// Count includes escaped spellings of that key. Missing fields have Count zero;
// non-string, non-null values are validated but never materialized.
type JSONStringField struct {
	Value    string
	Count    int
	IsString bool
	IsNull   bool
}

// ReadJSONTopLevelStrings validates one complete JSON document and extracts
// selected string fields. Unlike Decoder.Decode or Token, it does not buffer
// ignored values, including individual large strings and numbers. Memory is
// bounded by the reader buffer, nesting depth and the selected string values.
func ReadJSONTopLevelStrings(reader io.Reader, names ...string) (map[string]JSONStringField, error) {
	scanner := jsonFieldScanner{
		reader: bufio.NewReader(reader),
		fields: make(map[string]JSONStringField, len(names)),
	}
	for _, name := range names {
		scanner.fields[name] = JSONStringField{}
		// Each byte of a matching key can use at most one six-byte \u escape.
		scanner.keyLimit = max(scanner.keyLimit, len(name)*6+2)
	}
	if _, _, err := scanner.value(true, false, 0); err != nil {
		return nil, err
	}
	for {
		b, err := scanner.reader.ReadByte()
		if err == io.EOF {
			return scanner.fields, nil
		}
		if err != nil {
			return nil, err
		}
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return nil, ErrInvalidJSON
		}
	}
}

type jsonFieldScanner struct {
	reader   *bufio.Reader
	fields   map[string]JSONStringField
	keyLimit int
}

func (s *jsonFieldScanner) next() (byte, error) {
	for {
		b, err := s.reader.ReadByte()
		if err == io.EOF {
			return 0, ErrInvalidJSON
		}
		if err != nil {
			return 0, err
		}
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return b, nil
		}
	}
}

func (s *jsonFieldScanner) value(top, capture bool, depth int) (string, byte, error) {
	first, err := s.next()
	if err != nil {
		return "", 0, err
	}
	switch first {
	case '"':
		limit := 0
		if capture {
			limit = -1
		}
		raw, err := s.stringValue(limit)
		if err != nil || !capture {
			return "", first, err
		}
		var value string
		if err := UnmarshalJsonStr(raw, &value); err != nil {
			return "", first, ErrInvalidJSON
		}
		return value, first, nil
	case '{', '[':
		// Match the standard JSON codec's nesting bound, without recursing
		// indefinitely on hostile input that the downstream codec rejects.
		if depth >= 10000 {
			return "", first, ErrInvalidJSON
		}
		if first == '{' {
			return "", first, s.object(top, depth+1)
		}
		return "", first, s.array(depth + 1)
	case 'n', 't', 'f':
		tail := "ull"
		if first == 't' {
			tail = "rue"
		} else if first == 'f' {
			tail = "alse"
		}
		for i := range len(tail) {
			b, err := s.reader.ReadByte()
			if err != nil && err != io.EOF {
				return "", first, err
			}
			if err != nil || b != tail[i] {
				return "", first, ErrInvalidJSON
			}
		}
		return "", first, nil
	default:
		if first == '-' || first >= '0' && first <= '9' {
			return "", first, s.number(first)
		}
		return "", first, ErrInvalidJSON
	}
}

func (s *jsonFieldScanner) object(top bool, depth int) error {
	b, err := s.next()
	if err != nil || b == '}' {
		return err
	}
	for {
		if b != '"' {
			return ErrInvalidJSON
		}
		limit := 0
		if top {
			limit = s.keyLimit
		}
		rawKey, err := s.stringValue(limit)
		if err != nil {
			return err
		}
		var key string
		if rawKey != "" {
			if err := UnmarshalJsonStr(rawKey, &key); err != nil {
				return ErrInvalidJSON
			}
		}
		field, selected := s.fields[key]
		selected = selected && top && rawKey != ""
		if b, err = s.next(); err != nil {
			return err
		} else if b != ':' {
			return ErrInvalidJSON
		}
		value, kind, err := s.value(false, selected && field.Count == 0, depth)
		if err != nil {
			return err
		}
		if selected {
			if field.Count == 0 {
				field.Value, field.IsString, field.IsNull = value, kind == '"', kind == 'n'
			}
			field.Count++
			s.fields[key] = field
		}
		b, err = s.next()
		if err != nil || b == '}' {
			return err
		}
		if b != ',' {
			return ErrInvalidJSON
		}
		b, err = s.next()
		if err != nil {
			return err
		}
	}
}

func (s *jsonFieldScanner) array(depth int) error {
	b, err := s.next()
	if err != nil || b == ']' {
		return err
	}
	_ = s.reader.UnreadByte()
	for {
		if _, _, err := s.value(false, false, depth); err != nil {
			return err
		}
		b, err = s.next()
		if err != nil || b == ']' {
			return err
		}
		if b != ',' {
			return ErrInvalidJSON
		}
	}
}

// stringValue starts after the opening quote. A zero limit skips, a negative
// limit captures, and a positive limit drops keys too long to match a selector.
func (s *jsonFieldScanner) stringValue(limit int) (string, error) {
	var raw strings.Builder
	collect := limit != 0
	if collect {
		raw.WriteByte('"')
	}
	escaped, unicodeLeft := false, 0
	for {
		b, err := s.reader.ReadByte()
		if err == io.EOF {
			return "", ErrInvalidJSON
		}
		if err != nil {
			return "", err
		}
		end := false
		switch {
		case unicodeLeft > 0:
			if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
				return "", ErrInvalidJSON
			}
			unicodeLeft--
		case escaped:
			switch b {
			case 'u':
				unicodeLeft = 4
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			default:
				return "", ErrInvalidJSON
			}
			escaped = false
		case b == '\\':
			escaped = true
		case b == '"':
			end = true
		case b < 0x20:
			return "", ErrInvalidJSON
		}
		if collect {
			if limit > 0 && raw.Len() == limit {
				raw.Reset()
				collect = false
			} else {
				raw.WriteByte(b)
			}
		}
		if end {
			return raw.String(), nil
		}
	}
}

func (s *jsonFieldScanner) number(first byte) error {
	// States are the last significant part read, avoiding numeric conversion
	// or retention of arbitrarily long numeric literals.
	state := byte('i') // integer digits
	if first == '-' || first == '0' {
		state = first
	}
	for {
		p, err := s.reader.Peek(1)
		if err != nil && err != io.EOF {
			return err
		}
		b := byte(0)
		if err == nil {
			b = p[0]
		}
		digit := b >= '0' && b <= '9'
		switch state {
		case '-':
			if !digit {
				return ErrInvalidJSON
			}
			state = 'i'
			if b == '0' {
				state = '0'
			}
		case '0', 'i', 'f':
			switch {
			case digit && state != '0':
			case b == '.' && state != 'f':
				state = '.'
			case b == 'e' || b == 'E':
				state = 'e'
			default:
				return nil
			}
		case '.':
			if !digit {
				return ErrInvalidJSON
			}
			state = 'f'
		case 'e':
			if b == '+' || b == '-' {
				state = 's'
			} else if digit {
				state = 'x'
			} else {
				return ErrInvalidJSON
			}
		case 's':
			if !digit {
				return ErrInvalidJSON
			}
			state = 'x'
		case 'x':
			if !digit {
				return nil
			}
		}
		_, _ = s.reader.ReadByte()
	}
}
