package main

import (
	"strings"
	"testing"
)

func TestChooseDebugSwitch(t *testing.T) {
	tests := []struct {
		name        string
		names       []string
		interactive bool
		input       string
		want        string
		wantErr     bool
	}{
		{"none", nil, false, "", "", true},
		{"one", []string{"sw0"}, false, "", "sw0", false},
		{"multiple noninteractive", []string{"sw0", "sw1"}, false, "", "", true},
		{"multiple select", []string{"sw0", "sw1"}, true, "2\n", "sw1", false},
		{"invalid selection", []string{"sw0", "sw1"}, true, "9\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := chooseDebugSwitch(tt.names, tt.interactive, strings.NewReader(tt.input))
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("got %q, %v; want %q, err=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
