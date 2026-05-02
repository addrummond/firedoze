package main

import (
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
