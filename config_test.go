package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestParseShell(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    map[string]string
		wantErr bool
	}{
		{
			name:    "export prefix",
			content: "export FOO=bar\n",
			want:    map[string]string{"FOO": "bar"},
		},
		{
			name:    "no prefix",
			content: "FOO=bar\n",
			want:    map[string]string{"FOO": "bar"},
		},
		{
			name:    "export with extra whitespace",
			content: "  export   FOO=bar  \n",
			want:    map[string]string{"FOO": "bar"},
		},
		{
			name:    "double quoted value",
			content: `FOO="bar baz"` + "\n",
			want:    map[string]string{"FOO": "bar baz"},
		},
		{
			name:    "single quoted value",
			content: `FOO='bar baz'` + "\n",
			want:    map[string]string{"FOO": "bar baz"},
		},
		{
			name:    "comments and blank lines ignored",
			content: "# a comment\n\nFOO=bar\n   \n# another\nBAZ=qux\n",
			want:    map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name:    "later line overrides earlier",
			content: "FOO=first\nFOO=second\n",
			want:    map[string]string{"FOO": "second"},
		},
		{
			name:    "key with no export prefix but literally starts with export",
			content: "exportFOO=bar\n",
			want:    map[string]string{"exportFOO": "bar"},
		},
		{
			name:    "malformed line no equals",
			content: "export FOO\n",
			wantErr: true,
		},
		{
			name:    "malformed line empty key",
			content: "=bar\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseShell("test.env", []byte(tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got none (result: %v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseJSON(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    map[string]string
		wantErr bool
	}{
		{
			name:    "strings numbers bools null",
			content: `{"A":"a","N":3,"F":3.5,"B":true,"Z":false,"Nil":null}`,
			want:    map[string]string{"A": "a", "N": "3", "F": "3.5", "B": "true", "Z": "false", "Nil": ""},
		},
		{
			name:    "nested object is hard error",
			content: `{"A":{"nested":"value"}}`,
			wantErr: true,
		},
		{
			name:    "nested array is hard error",
			content: `{"A":[1,2,3]}`,
			wantErr: true,
		},
		{
			name:    "top level array is hard error",
			content: `[1,2,3]`,
			wantErr: true,
		},
		{
			name:    "invalid json is hard error",
			content: `{not valid json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJSON("test.json", []byte(tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got none (result: %v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name string
		path string
		data string
		want string
	}{
		{"by extension", "config.json", "FOO=bar", "json"},
		{"by content sniff brace", "config", `{"FOO":"bar"}`, "json"},
		{"by content sniff leading whitespace", "config", "  \n  {\"FOO\":\"bar\"}", "json"},
		{"defaults to shell", "config", "FOO=bar", "shell"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectFormat(tt.path, []byte(tt.data))
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveEntry(t *testing.T) {
	dir := t.TempDir()

	literal := filepath.Join(dir, "config")
	if err := os.WriteFile(literal, []byte("FOO=bar"), 0o644); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(dir, "does-not-exist")

	// glob matching N files
	for _, name := range []string{"a.env", "b.env"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("X=1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a directory that would otherwise match the glob
	if err := os.Mkdir(filepath.Join(dir, "c.env"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("literal path", func(t *testing.T) {
		got, err := resolveEntry(literal)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, []string{literal}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("literal missing path is soft", func(t *testing.T) {
		got, err := resolveEntry(missing)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("expected no files, got %v", got)
		}
	})

	t.Run("glob matches N files sorted, skips directory", func(t *testing.T) {
		got, err := resolveEntry(filepath.Join(dir, "*.env"))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(dir, "a.env"), filepath.Join(dir, "b.env")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("glob matches zero files is soft", func(t *testing.T) {
		got, err := resolveEntry(filepath.Join(dir, "nope-*.env"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("expected no files, got %v", got)
		}
	})
}

func TestLoadSecretsMultiSourceMergeOrdering(t *testing.T) {
	dir := t.TempDir()

	first := filepath.Join(dir, "first.env")
	second := filepath.Join(dir, "second.env")
	if err := os.WriteFile(first, []byte("A=1\nB=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("B=2\nC=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := loadSecrets([]string{first, second}, "auto")
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"A": "1", "B": "2", "C": "2"}
	if !reflect.DeepEqual(result.Values, want) {
		t.Fatalf("got %v, want %v", result.Values, want)
	}
}

func TestLoadSecretsTracksMissingEntries(t *testing.T) {
	dir := t.TempDir()

	present := filepath.Join(dir, "present.env")
	if err := os.WriteFile(present, []byte("FOO=bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.json")

	result, err := loadSecrets([]string{present, missing}, "auto")
	if err != nil {
		t.Fatal(err)
	}

	wantValues := map[string]string{"FOO": "bar"}
	if !reflect.DeepEqual(result.Values, wantValues) {
		t.Fatalf("got values %v, want %v", result.Values, wantValues)
	}
	wantMissing := []string{missing}
	if !reflect.DeepEqual(result.Missing, wantMissing) {
		t.Fatalf("got missing %v, want %v", result.Missing, wantMissing)
	}
}

func TestLoadSecretsNotFoundIsSoft(t *testing.T) {
	dir := t.TempDir()
	result, err := loadSecrets([]string{
		filepath.Join(dir, "missing-literal"),
		filepath.Join(dir, "missing-*.env"),
	}, "auto")
	if err != nil {
		t.Fatalf("expected soft not-found, got error: %v", err)
	}
	if len(result.Values) != 0 {
		t.Fatalf("expected no values, got %v", result.Values)
	}
}

func TestLoadSecretsMalformedContentIsHardError(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("NOT_A_VALID_LINE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSecrets([]string{bad}, "auto"); err == nil {
		t.Fatal("expected hard error for malformed content")
	}
}

func TestBuildEnvironDeduplicatesAndOverrides(t *testing.T) {
	t.Setenv("EXISTING_VAR", "inherited")
	secrets := map[string]string{
		"EXISTING_VAR": "overridden",
		"NEW_SECRET":   "value",
	}

	env := buildEnviron(secrets)

	seen := map[string]string{}
	counts := map[string]int{}
	for _, kv := range env {
		idx := -1
		for i, c := range kv {
			if c == '=' {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("malformed env entry %q", kv)
		}
		k, v := kv[:idx], kv[idx+1:]
		counts[k]++
		seen[k] = v
	}

	for k, c := range counts {
		if c != 1 {
			t.Fatalf("key %q appears %d times in envp, want exactly 1", k, c)
		}
	}
	if seen["EXISTING_VAR"] != "overridden" {
		t.Fatalf("expected secret to override inherited var, got %q", seen["EXISTING_VAR"])
	}
	if seen["NEW_SECRET"] != "value" {
		t.Fatalf("expected new secret present, got %q", seen["NEW_SECRET"])
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]string{"b": "1", "a": "1", "c": "1"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("not sorted: %v", got)
	}
}
