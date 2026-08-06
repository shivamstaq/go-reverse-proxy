package main

import "testing"

func TestGetenv(t *testing.T) {
	if got := getenv("UPSTREAM_TEST_UNSET_VAR", "fallback"); got != "fallback" {
		t.Errorf("getenv(unset) = %q, want fallback", got)
	}

	t.Setenv("UPSTREAM_TEST_VAR", "explicit")
	if got := getenv("UPSTREAM_TEST_VAR", "fallback"); got != "explicit" {
		t.Errorf("getenv(set) = %q, want explicit", got)
	}

	// An empty value must fall back rather than yield "".
	t.Setenv("UPSTREAM_TEST_EMPTY", "")
	if got := getenv("UPSTREAM_TEST_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("getenv(empty) = %q, want fallback", got)
	}
}
