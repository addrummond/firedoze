package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONTextMarshalJSON(t *testing.T) {
	tests := []struct {
		name  string
		value JSONText
		want  string
	}{
		{name: "empty", value: "", want: "null"},
		{name: "object", value: `{"rootfs":{"basename":"rootfs.ext4"}}`, want: `{"rootfs":{"basename":"rootfs.ext4"}}`},
		{name: "array", value: `[1,2,3]`, want: `[1,2,3]`},
		{name: "invalid becomes string", value: `not-json`, want: `"not-json"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("MarshalJSON = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestJSONTextUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		data string
		want JSONText
	}{
		{name: "null", data: `null`, want: ""},
		{name: "object", data: `{"rootfs":{"basename":"rootfs.ext4"}}`, want: `{"rootfs":{"basename":"rootfs.ext4"}}`},
		{name: "string", data: `"plain text"`, want: `"plain text"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := JSONText(`{"old":true}`)
			if err := json.Unmarshal([]byte(tt.data), &value); err != nil {
				t.Fatal(err)
			}
			if value != tt.want {
				t.Fatalf("UnmarshalJSON = %s, want %s", value, tt.want)
			}
		})
	}

	var value JSONText
	if err := json.Unmarshal([]byte(`not-json`), &value); err == nil {
		t.Fatal("json.Unmarshal accepted invalid JSON")
	}
	if err := value.UnmarshalJSON([]byte(`not-json`)); err == nil || !strings.Contains(err.Error(), "invalid JSONText") {
		t.Fatalf("direct UnmarshalJSON invalid error = %v, want invalid JSONText", err)
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
		{name: "invalid json string", value: `not-json`, want: `not-json`},
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
	if err := value.Scan(42); err == nil || !strings.Contains(err.Error(), "scan JSONText from int") {
		t.Fatalf("Scan int error = %v, want type error", err)
	}
}
