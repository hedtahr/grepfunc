package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManifestDeclaresMojo(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		body     string
		want     bool
	}{
		{"pixi dependency", "pixi.toml", "[dependencies]\nmojo = \">=1.0\"\n", true},
		{"pep 621 dependency", "pyproject.toml", "dependencies = [\n  \"mojo>=1.0\",\n]\n", true},
		{"no mojo", "pixi.toml", "[dependencies]\npython = \"*\"\n", false},
		{"substring is not a dependency", "pixi.toml", "[dependencies]\nmojolicious = \"*\"\n", false},
		{"missing manifest", "pixi.toml", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.body != "" {
				err := os.WriteFile(filepath.Join(dir, tt.manifest), []byte(tt.body), 0600)
				if err != nil {
					t.Fatal(err)
				}
			}

			if got := manifestDeclaresMojo(dir, tt.manifest); got != tt.want {
				t.Errorf("manifestDeclaresMojo(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestContainsWord(t *testing.T) {
	tests := []struct {
		data string
		want bool
	}{
		{"mojo", true},
		{"mojo = \"*\"", true},
		{"\"mojo>=1.0\"", true},
		{"mojolicious", false},
		{"python", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := containsWord([]byte(tt.data), "mojo"); got != tt.want {
			t.Errorf("containsWord(%q) = %v, want %v", tt.data, got, tt.want)
		}
	}
}
