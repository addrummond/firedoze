package main

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{name: "seconds", duration: 12 * time.Second, want: "12s"},
		{name: "minutes", duration: 3*time.Minute + 4*time.Second, want: "3m4s"},
		{name: "hours", duration: 5*time.Hour + 6*time.Minute + 7*time.Second, want: "5h6m"},
		{name: "days", duration: 2*24*time.Hour + 3*time.Hour + 4*time.Minute, want: "2d3h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatDuration(tt.duration); got != tt.want {
				t.Fatalf("formatDuration(%s) = %q, want %q", tt.duration, got, tt.want)
			}
		})
	}
}

func TestParseNamesAndFlags(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	memoryMiB := flags.Int("memory-mib", 0, "")
	diskBytes := flags.Int64("disk-bytes", 0, "")

	names, err := parseNamesAndFlags(flags, []string{"alice", "bob", "--memory-mib", "512", "--disk-bytes=1024"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(names), 2; got != want {
		t.Fatalf("len(names) = %d, want %d", got, want)
	}
	if names[0] != "alice" || names[1] != "bob" {
		t.Fatalf("names = %#v", names)
	}
	if *memoryMiB != 512 {
		t.Fatalf("memoryMiB = %d, want 512", *memoryMiB)
	}
	if *diskBytes != 1024 {
		t.Fatalf("diskBytes = %d, want 1024", *diskBytes)
	}
}

func TestWGKeygen(t *testing.T) {
	oldStdout := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	err = (app{}).wg([]string{"keygen"})
	_ = write.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	if !strings.Contains(output, "private_key = ") || !strings.Contains(output, "public_key = ") {
		t.Fatalf("keygen output missing keys:\n%s", output)
	}
}
