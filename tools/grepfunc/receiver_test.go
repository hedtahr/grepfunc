package grepfunc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The receiver clause is Go's, and every form is legal: with a receiver name,
// with a pointer, with type parameters, or with none of those.
func TestFilterByReceiver(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"named pointer receiver", "func (s *Server) Handle(req Request) *Response {", true},
		{"unnamed pointer receiver", "func (*Server) Handle() {", true},
		{"value receiver", "func (s Server) Handle() {", true},
		{"pointer with type params", "func (s *Server[T]) Handle() {", true},
		{"multiple type params", "func (s *Server[T, U]) Handle() {", true},
		{"other receiver type", "func (t *Other) Handle() {", false},
		{"plain function taking the type", "func Handle(s *Server) {", false},
		{"conversion inside another body", "func do() {\n\tx := (*Server)(nil)\n\t_ = x\n}", false},
		{"method with conversion in its own body", "func (s *Server) Handle() {\n\tx := (*Server)(nil)\n}", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterByReceiver([]FuncMatch{{Body: tt.body}}, "Server")
			if (len(got) == 1) != tt.want {
				t.Errorf("filterByReceiver(%q) matched = %v, want %v", tt.body, len(got) == 1, tt.want)
			}
		})
	}
}

// Receiver filtering must work in the two modes that callers use, including
// names_only, where bodies used to be skipped and the filter then matched nothing.
func TestHandleReceiverFilter(t *testing.T) {
	dir := t.TempDir()
	code := `package p

type Server struct{}

type Other struct{}

func (s *Server) Handle(req Request) *Response {
	return nil
}

func (o *Other) Handle(req Request) {}

func Plain() {}
`

	err := os.WriteFile(filepath.Join(dir, "server.go"), []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	for _, namesOnly := range []bool{false, true} {
		raw, err := json.Marshal(map[string]any{
			schemaPattern: "Handle",
			schemaPath:    dir,
			schemaInclude: globGo,
			"receiver":    "Server",
			"names_only":  namesOnly,
		})
		if err != nil {
			t.Fatal(err)
		}

		result, err := Handle(raw)
		if err != nil {
			t.Fatal(err)
		}

		text := result.Content[0].Text
		// Fixture lines: the Server method is 7, the Other method 11, the plain
		// function 13. Only line 7 may survive the receiver filter.
		if !strings.Contains(text, "server.go:7") {
			t.Errorf("names_only=%v: output missing the Server method at line 7:\n%s", namesOnly, text)
		}

		for _, unwanted := range []string{"server.go:11", "server.go:13"} {
			if strings.Contains(text, unwanted) {
				t.Errorf("names_only=%v: output contains %s, want only the Server method:\n%s", namesOnly, unwanted, text)
			}
		}
	}
}
