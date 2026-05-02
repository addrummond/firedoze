package store

import (
	"encoding/json"
	"testing"
)

func TestJSONTextMarshalJSON(t *testing.T) {
	value := struct {
		Metadata JSONText `json:"metadata,omitempty"`
	}{
		Metadata: JSONText(`{"rootfs":{"basename":"rootfs.ext4"}}`),
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"metadata":{"rootfs":{"basename":"rootfs.ext4"}}}`
	if string(data) != want {
		t.Fatalf("json.Marshal = %s, want %s", data, want)
	}
}
