package catalogservice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

func decodeStrictJSON(data []byte, limit int64, name string, value any) error {
	if int64(len(data)) > limit {
		return fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if err := validateUniqueJSON(data); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	if err := validateExactJSONFields(data, reflect.TypeOf(value), "$", name); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode %s: multiple JSON values", name)
		}
		return fmt.Errorf("decode %s trailing data: %w", name, err)
	}
	return nil
}

func validateExactJSONFields(data []byte, expected reflect.Type, path, name string) error {
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var walk func(any, reflect.Type, string) error
	walk = func(value any, typ reflect.Type, current string) error {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ == reflect.TypeOf(json.RawMessage{}) || typ.Kind() == reflect.Interface {
			return nil
		}
		switch actual := value.(type) {
		case map[string]any:
			if typ.Kind() == reflect.Map {
				for key, child := range actual {
					if err := walk(child, typ.Elem(), current+"."+key); err != nil {
						return err
					}
				}
				return nil
			}
			if typ.Kind() != reflect.Struct {
				return nil
			}
			fields := map[string]reflect.Type{}
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				tag := field.Tag.Get("json")
				jsonName := strings.Split(tag, ",")[0]
				if jsonName == "-" {
					continue
				}
				if jsonName == "" {
					jsonName = field.Name
				}
				fields[jsonName] = field.Type
			}
			for key, child := range actual {
				fieldType, ok := fields[key]
				if !ok {
					return fmt.Errorf("decode %s: json: unknown field %q at %s", name, key, current)
				}
				if err := walk(child, fieldType, current+"."+key); err != nil {
					return err
				}
			}
		case []any:
			if typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
				for i, child := range actual {
					if err := walk(child, typ.Elem(), fmt.Sprintf("%s[%d]", current, i)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return walk(decoded, expected, path)
}

func validateUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkUniqueJSONValue(decoder, "$", nil); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected trailing JSON token %v", token)
	}
	return nil
}

func walkUniqueJSONValue(decoder *json.Decoder, path string, first json.Token) error {
	token := first
	var err error
	if token == nil {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object at %s has a non-string key", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON member %q at %s", key, path)
			}
			seen[key] = struct{}{}
			if err := walkUniqueJSONValue(decoder, path+"."+key, nil); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("JSON object at %s has invalid closing token", path)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkUniqueJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index), nil); err != nil {
				return err
			}
			index++
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("JSON array at %s has invalid closing token", path)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, path)
	}
	return nil
}
