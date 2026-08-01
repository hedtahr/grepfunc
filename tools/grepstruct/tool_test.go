package grepstruct

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

func TestHandle(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "types.go")

	code := `package test

type User struct {
	Name  string
	Email string
}

type Config struct {
	Port int
}
`

	err := os.WriteFile(filePath, []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		keyPattern:    keyName,
		keyPath:       dir,
		keyGoGlob:     keyGoGlob,
		"max_results": 5,
		keyBody:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	if !strings.Contains(text, "User") {
		t.Error("output should contain User struct")
	}

	if !strings.Contains(text, "\x60\x60\x60") {
		t.Error("output should contain code blocks when body:true")
	}
}

func TestBodyFalseSignatureOnly(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "types.go")

	err := os.WriteFile(filePath, []byte("package test\ntype User struct {\n\tName string\n}\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		keyPattern: keyName,
		keyPath:    dir,
		keyGoGlob:  "*.go",
		keyBody:    false,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	if text == "" {
		t.Fatal("empty output")
	}

	if !strings.Contains(text, "\x60\x60\x60") {
		t.Error("body=false should contain a code fence wrapper")
	}

	if !strings.Contains(text, "User") {
		t.Error("should still contain struct name")
	}
}

func TestBodyTrueIncludesBody(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "types.go")

	err := os.WriteFile(filePath, []byte("package test\ntype User struct {\n\tName string\n}\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		keyPattern: keyName,
		keyPath:    dir,
		keyGoGlob:  "*.go",
		keyBody:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	if !strings.Contains(text, "\x60\x60\x60") {
		t.Error("body=true should contain code blocks")
	}
}

func TestIsStructSig(t *testing.T) {
	sigs := []string{
		"type User struct {",                // Go struct
		"type Handler interface {",          // Go interface
		"struct Node {",                     // Rust struct
		"enum Status {",                     // Rust enum
		"trait Serialize {",                 // Rust trait
		"impl Handler {",                    // Rust impl
		"pub struct PublicNode {",           // Rust pub struct
		"pub enum Color {",                  // Rust pub enum
		"class UserService {",               // JS/TS class
		"interface Repository {",            // TS interface
		"type Config = {",                   // TS type alias
		"export class ApiClient {",          // TS export class
		"export interface IHandler {",       // TS export interface
		"public class Controller {",         // Java class
		"private interface Internal {",      // Java interface
		"public enum Result {",              // Java enum
		"data class User(val name: String)", // Kotlin data class
		"object Singleton {",                // Kotlin object
		"sealed class State {",              // Kotlin sealed class
		"actor Worker {",                    // Swift actor
		"protocol Drawable {",               // Swift protocol
		"extension String {",                // Swift extension
	}
	for _, sig := range sigs {
		if !grepfunc.IsStructSig([]byte(sig)) {
			t.Errorf("IsStructSig(%q) = false, want true", sig)
		}
	}

	nonSigs := []string{
		"func Hello() {",
		"fn process() {",
		"def hello():",
		"if x > 0 {",
		"for i := 0; i < n; i++ {",
		"var x = 1",
		"const y = 2",
		"import { foo } from 'bar'",
	}
	for _, sig := range nonSigs {
		if grepfunc.IsStructSig([]byte(sig)) {
			t.Errorf("IsStructSig(%q) = true, want false", sig)
		}
	}
}
