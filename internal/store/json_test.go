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

func TestJSONTextUnmarshalJSON(t *testing.T) {
	var value struct {
		Metadata JSONText `json:"metadata,omitempty"`
	}
	if err := json.Unmarshal([]byte(`{"metadata":{"rootfs":{"basename":"rootfs.ext4"}}}`), &value); err != nil {
		t.Fatal(err)
	}
	const want = `{"rootfs":{"basename":"rootfs.ext4"}}`
	if string(value.Metadata) != want {
		t.Fatalf("Metadata = %s, want %s", value.Metadata, want)
	}
}

func TestJSONTextUnmarshalNull(t *testing.T) {
	var value struct {
		Metadata JSONText `json:"metadata,omitempty"`
	}
	value.Metadata = JSONText(`{"old":true}`)
	if err := json.Unmarshal([]byte(`{"metadata":null}`), &value); err != nil {
		t.Fatal(err)
	}
	if value.Metadata != "" {
		t.Fatalf("Metadata = %s, want empty", value.Metadata)
	}
}
