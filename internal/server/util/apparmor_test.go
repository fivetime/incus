package util

import "testing"

func TestNormalizeAppArmorProfile(t *testing.T) {
	tests := map[string]string{
		"":                           "",
		"unconfined\n":               "unconfined",
		"runc (unconfined)\n":        "unconfined",
		"docker-default (enforce)\n": "docker-default (enforce)",
	}

	for input, expected := range tests {
		actual := normalizeAppArmorProfile(input)
		if actual != expected {
			t.Fatalf("Expected %q for %q, got %q", expected, input, actual)
		}
	}
}
