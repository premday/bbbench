package main

import "testing"

func TestVersionIsSet(t *testing.T) {
	if version == "" {
		t.Fatal("version must not be empty; release builds override it with -ldflags -X main.version")
	}
}
