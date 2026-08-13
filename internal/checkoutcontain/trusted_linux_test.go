//go:build linux

package checkoutcontain

import (
	"os"
	"testing"
)

func TestPathAndRelativeNameRejection(t *testing.T) {
	for _, path := range []string{"", ".", "relative", "/", "/tmp/x\x00y"} {
		if validAbsolute(path) {
			t.Fatalf("unsafe absolute path accepted: %q", path)
		}
	}
	for _, name := range []string{"", ".", "..", "a/b", "a\x00b"} {
		if validRelativeName(name) {
			t.Fatalf("unsafe relative name accepted: %q", name)
		}
	}
	if !validAbsolute("/var/lib/autodeploy") || !validRelativeName("git") {
		t.Fatal("safe paths rejected")
	}
}

func TestELFValidationRejectsScriptsAndAcceptsMinimalELFHeader(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "candidate")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	if isELF(int(f.Fd())) {
		t.Fatal("script accepted as executable ELF")
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if !isELF(int(f.Fd())) {
		t.Fatal("valid ELF header rejected")
	}
}
