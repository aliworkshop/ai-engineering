package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// ReadFile returns a file's contents. Read-only, so it never asks for approval.
type ReadFile struct{}

func (ReadFile) Spec() openai.Tool {
	return defineTool("read_file", "Read a file from disk and return its contents.",
		`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
}

func (ReadFile) Run(_ context.Context, args string) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	b, err := os.ReadFile(a.Path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WriteFile creates or overwrites a file. Dangerous — gated by the Approver.
type WriteFile struct{ Approver Approver }

func (WriteFile) Spec() openai.Tool {
	return defineTool("write_file", "Create or overwrite a file on disk. Requires human approval.",
		`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)
}

func (t WriteFile) Run(_ context.Context, args string) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if !t.Approver.Confirm(fmt.Sprintf("WRITE file %q (%d bytes)", a.Path, len(a.Content))) {
		return "Denied by user; file not written.", nil
	}
	if dir := filepath.Dir(a.Path); dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(a.Path, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return "Wrote " + a.Path, nil
}

// EditFile replaces the first occurrence of OldString with NewString in an
// existing file. Dangerous — gated by the Approver.
type EditFile struct{ Approver Approver }

func (EditFile) Spec() openai.Tool {
	return defineTool("edit_file",
		"Replace the first occurrence of old_string with new_string in an existing file. Requires human approval.",
		`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["path","old_string","new_string"]}`)
}

func (t EditFile) Run(_ context.Context, args string) (string, error) {
	var a struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	original, err := os.ReadFile(a.Path)
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(original), a.OldString) {
		return "old_string not found in file; nothing changed.", nil
	}
	if !t.Approver.Confirm(fmt.Sprintf("EDIT file %q (replace %q with %q)", a.Path, a.OldString, a.NewString)) {
		return "Denied by user; file not changed.", nil
	}
	updated := strings.Replace(string(original), a.OldString, a.NewString, 1)
	if err := os.WriteFile(a.Path, []byte(updated), 0o644); err != nil {
		return "", err
	}
	return "Edited " + a.Path, nil
}

// DeleteFile removes a file. Dangerous — gated by the Approver.
type DeleteFile struct{ Approver Approver }

func (DeleteFile) Spec() openai.Tool {
	return defineTool("delete_file", "Delete a file from disk. Requires human approval.",
		`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
}

func (t DeleteFile) Run(_ context.Context, args string) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if !t.Approver.Confirm(fmt.Sprintf("DELETE file %q", a.Path)) {
		return "Denied by user; file not deleted.", nil
	}
	if err := os.Remove(a.Path); err != nil {
		return "", err
	}
	return "Deleted " + a.Path, nil
}
