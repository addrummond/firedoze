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

func TestJSONTextMarshalInvalidJSONAsString(t *testing.T) {
	data, err := json.Marshal(JSONText(`not-json`))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"not-json"` {
		t.Fatalf("json.Marshal = %s, want quoted string", data)
	}
}

func TestJSONTextUnmarshalInvalidJSON(t *testing.T) {
	var value JSONText
	if err := json.Unmarshal([]byte(`not-json`), &value); err == nil {
		t.Fatal("Unmarshal invalid JSON succeeded")
	}
}

func TestJSONTextScan(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  JSONText
	}{
		{name: "nil", value: nil, want: ""},
		{name: "string", value: `{"a":1}`, want: `{"a":1}`},
		{name: "bytes", value: []byte(`{"b":2}`), want: `{"b":2}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var value JSONText
			if err := value.Scan(tt.value); err != nil {
				t.Fatal(err)
			}
			if value != tt.want {
				t.Fatalf("Scan = %s, want %s", value, tt.want)
			}
		})
	}

	var value JSONText
	if err := value.Scan(42); err == nil {
		t.Fatal("Scan int succeeded")
	}
}
