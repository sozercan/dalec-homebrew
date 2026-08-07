package fetcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func rejectDuplicateJSONMembers(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			memberToken, err := decoder.Token()
			if err != nil {
				return err
			}
			member, ok := memberToken.(string)
			if !ok {
				return errors.New("JSON object member is not a string")
			}
			if _, exists := seen[member]; exists {
				return fmt.Errorf("duplicate JSON member %q", member)
			}
			seen[member] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("invalid JSON object terminator")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("invalid JSON array terminator")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func requireExactTopLevelMembers(data []byte, allowed map[string]struct{}) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errors.New("expected a JSON object")
	}
	for key := range object {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("json: unknown field %q", key)
		}
	}
	return nil
}
