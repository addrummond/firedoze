package store

import (
	"encoding/json"
	"fmt"
)

type JSONText string

func (j JSONText) MarshalJSON() ([]byte, error) {
	if j == "" {
		return []byte("null"), nil
	}
	data := []byte(j)
	if json.Valid(data) {
		return data, nil
	}
	return json.Marshal(string(j))
}

func (j *JSONText) Scan(value any) error {
	switch value := value.(type) {
	case nil:
		*j = ""
	case string:
		*j = JSONText(value)
	case []byte:
		*j = JSONText(value)
	default:
		return fmt.Errorf("scan JSONText from %T", value)
	}
	return nil
}
