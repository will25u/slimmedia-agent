package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// DefaultMaxJSONDepth is the maximum allowable JSON object/array nesting depth
// for protocol payloads to prevent stack exhaustion attacks.
const DefaultMaxJSONDepth = 64

// CheckJSONDepth verifies that data contains valid JSON that does not exceed
// maxDepth levels of object or array nesting. If maxDepth <= 0, DefaultMaxJSONDepth is used.
func CheckJSONDepth(data []byte, maxDepth int) error {
	if len(data) == 0 {
		return nil
	}
	if maxDepth <= 0 {
		maxDepth = DefaultMaxJSONDepth
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
				if depth > maxDepth {
					return fmt.Errorf("json nesting depth exceeds maximum allowed limit (%d)", maxDepth)
				}
			case '}', ']':
				if depth > 0 {
					depth--
				}
			}
		}
	}
	return nil
}

// UnmarshalWithDepthLimit validates the JSON nesting depth before unmarshaling into v.
func UnmarshalWithDepthLimit(data []byte, v interface{}, maxDepth int) error {
	if err := CheckJSONDepth(data, maxDepth); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
