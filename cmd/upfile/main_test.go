package main

import (
	"strconv"
	"strings"
	"testing"
)

func TestNativeInteger(t *testing.T) {
	t.Run("fallback", func(t *testing.T) {
		t.Setenv("UPFILE_TEST_INTEGER", "")
		got, err := nativeInteger("UPFILE_TEST_INTEGER", 42)
		if err != nil {
			t.Fatal(err)
		}
		if got != 42 {
			t.Fatalf("nativeInteger() = %d, want 42", got)
		}
	})

	t.Run("configured value", func(t *testing.T) {
		t.Setenv("UPFILE_TEST_INTEGER", "17")
		got, err := nativeInteger("UPFILE_TEST_INTEGER", 42)
		if err != nil {
			t.Fatal(err)
		}
		if got != 17 {
			t.Fatalf("nativeInteger() = %d, want 17", got)
		}
	})

	for name, value := range map[string]string{
		"invalid":  "not-a-number",
		"overflow": "1" + strings.Repeat("0", strconv.IntSize),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("UPFILE_TEST_INTEGER", value)
			if _, err := nativeInteger("UPFILE_TEST_INTEGER", 42); err == nil {
				t.Fatal("nativeInteger() error = nil, want error")
			}
		})
	}
}
