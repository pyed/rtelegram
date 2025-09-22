package main

import "testing"

func TestProcessOptions(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantDir   string
		wantLabel string
	}{
		{
			name:      "empty input",
			input:     "",
			wantDir:   "",
			wantLabel: "",
		},
		{
			name:      "explicit dir and label",
			input:     "d=/downloads l=Media",
			wantDir:   "/downloads",
			wantLabel: "Media",
		},
		{
			name:      "reversed order",
			input:     "l=Movies d=/srv",
			wantDir:   "/srv",
			wantLabel: "Movies",
		},
		{
			name:      "implicit directory",
			input:     "/mnt/storage Action",
			wantDir:   "/mnt/storage",
			wantLabel: "Action",
		},
		{
			name:      "windows style path",
			input:     "d=C:/Downloads l=Games",
			wantDir:   "C:/Downloads",
			wantLabel: "Games",
		},
		{
			name:      "fallback label",
			input:     "d=/downloads Label",
			wantDir:   "/downloads",
			wantLabel: "Label",
		},
		{
			name:      "label override",
			input:     "l=Initial Final",
			wantDir:   "",
			wantLabel: "Final",
		},
		{
			name:      "directory override",
			input:     "/first/path /second/path",
			wantDir:   "/second/path",
			wantLabel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDir, gotLabel := processOptions(tt.input)
			if gotDir != tt.wantDir || gotLabel != tt.wantLabel {
				t.Fatalf("processOptions(%q) = (%q, %q), want (%q, %q)", tt.input, gotDir, gotLabel, tt.wantDir, tt.wantLabel)
			}
		})
	}
}

func TestAMaster(t *testing.T) {
	Masters = []string{"alice", "bob"}
	t.Cleanup(func() { Masters = nil })

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "exact match", input: "alice", want: true},
		{name: "case insensitive", input: "Bob", want: true},
		{name: "unknown", input: "carol", want: false},
		{name: "empty", input: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := aMaster(tt.input); got != tt.want {
				t.Fatalf("aMaster(%q) = %t, want %t", tt.input, got, tt.want)
			}
		})
	}
}

func TestMDReplacer(t *testing.T) {
	input := "*value* with *stars*"
	want := "•value• with •stars•"
	if got := mdReplacer.Replace(input); got != want {
		t.Fatalf("mdReplacer.Replace(%q) = %q, want %q", input, got, want)
	}
}
