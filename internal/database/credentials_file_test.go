package database

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds")
	if err := os.WriteFile(path, []byte("dynuser\ndynpass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, p, err := readCredentialsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if u != "dynuser" || p != "dynpass" {
		t.Fatalf("got %q/%q", u, p)
	}
}

func TestReadCredentialsFile_rejectsShort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds")
	if err := os.WriteFile(path, []byte("onlyuser\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCredentialsFile(path); err == nil {
		t.Fatal("expected error")
	}
}
