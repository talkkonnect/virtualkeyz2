package main

import "testing"

func TestTechMenuTabCompleteLine(t *testing.T) {
	tests := []struct {
		in, want string
		bell     bool
	}{
		{"cf", "cfg ", false},
		{"cfg ", "cfg ", true},
		{"cfg l", "cfg li", false},
		{"cfg lis", "cfg list ", false},
		{"cfg set log_le", "cfg set log_level ", false},
		{"kb ", "kb all ", false},
		{"9", "9 ", false},
	}
	for _, tc := range tests {
		got, bell := techMenuTabCompleteLine(tc.in)
		if bell != tc.bell || got != tc.want {
			t.Errorf("TabComplete(%q) = (%q, %v); want (%q, %v)", tc.in, got, bell, tc.want, tc.bell)
		}
	}
}
