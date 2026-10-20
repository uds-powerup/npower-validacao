package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestResolveDataDir(t *testing.T) {
	home := t.TempDir()
	userHome := func() (string, error) { return home, nil }
	fallback := filepath.Join(home, ".npower-validacao")

	good := filepath.Join(t.TempDir(), "data", "kit")
	notWritable := filepath.Join(t.TempDir(), "file")
	os.WriteFile(notWritable, []byte("x"), 0o644) // a file where a directory is needed

	cases := []struct {
		name       string
		env        map[string]string
		wantDir    string
		wantSource string
		wantReason string
	}{
		{"uses PLUGIN_DATA and creates it", map[string]string{"PLUGIN_DATA": good}, good, "PLUGIN_DATA", ""},
		{"unset falls back", nil, fallback, "fallback", "not set"},
		{"unexpanded placeholder falls back", map[string]string{"PLUGIN_DATA": "${PLUGIN_DATA}/x"}, fallback, "fallback", "not expanded"},
		{"relative falls back", map[string]string{"PLUGIN_DATA": "relative/dir"}, fallback, "fallback", "not absolute"},
		{"uncreatable falls back", map[string]string{"PLUGIN_DATA": filepath.Join(notWritable, "sub")}, fallback, "fallback", "cannot be created"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, source, reason := resolveDataDir(envOf(tc.env), userHome)
			if dir != tc.wantDir || source != tc.wantSource || !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, ~%q)", dir, source, reason, tc.wantDir, tc.wantSource, tc.wantReason)
			}
		})
	}
	if info, err := os.Stat(good); err != nil || !info.IsDir() {
		t.Errorf("PLUGIN_DATA was not created: %v", err)
	}
}

func TestResolveDataDirWithoutHome(t *testing.T) {
	noHome := func() (string, error) { return "", errors.New("no home") }
	dir, source, _ := resolveDataDir(envOf(nil), noHome)
	if source != "fallback" || !strings.HasPrefix(dir, os.TempDir()) {
		t.Fatalf("got (%q, %q), want a fallback under the temp dir", dir, source)
	}
}
