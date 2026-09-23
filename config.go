package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadResult is the outcome of loading zero or more secrets config sources.
type LoadResult struct {
	// Values holds the merged KEY -> VALUE pairs, later sources winning on
	// name collision.
	Values map[string]string
	// Searched lists every configured entry (literal path or glob
	// pattern), in the order they were processed. Used to describe a
	// SecretsNotFound event when Values ends up empty.
	Searched []string
	// Missing lists the configured entries (literal paths or glob
	// patterns), in processing order, that matched zero files -- a
	// literal path that doesn't exist, or a pattern with no matches.
	// Used to note partial misses in an otherwise-successful
	// SecretsLoaded event.
	Missing []string
}

// loadSecrets resolves every configured entry (literal path or glob
// pattern) in order and merges their contents into a single key/value
// map. A literal path that does not exist, or a pattern matching zero
// files, is not an error: it simply contributes nothing (soft case, see
// spec §6). Any other error (malformed content, bad glob syntax, an
// unreadable existing file) is fatal and returned as-is.
func loadSecrets(entries []string, format string) (*LoadResult, error) {
	result := &LoadResult{
		Values:   make(map[string]string),
		Searched: append([]string(nil), entries...),
	}

	for _, entry := range entries {
		files, err := resolveEntry(entry)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			result.Missing = append(result.Missing, entry)
		}
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", file, err)
			}

			fileFormat := format
			if fileFormat == "auto" {
				fileFormat = detectFormat(file, data)
			}

			var parsed map[string]string
			if fileFormat == "json" {
				parsed, err = parseJSON(file, data)
			} else {
				parsed, err = parseShell(file, data)
			}
			if err != nil {
				return nil, err
			}

			for k, v := range parsed {
				result.Values[k] = v
			}
		}
	}

	return result, nil
}

// resolveEntry expands a single configured entry into the regular files
// it refers to, in read order. Directories (whether the literal path
// itself or a glob match) are silently skipped: not read, not counted as
// found, not counted as missing.
func resolveEntry(entry string) ([]string, error) {
	if !isGlob(entry) {
		info, err := os.Stat(entry)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("stat %s: %w", entry, err)
		}
		if info.IsDir() {
			return nil, nil
		}
		return []string{entry}, nil
	}

	matches, err := filepath.Glob(entry)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern %q: %w", entry, err)
	}

	files := make([]string, 0, len(matches))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			// Vanished between Glob and Stat: treat like "not matched".
			continue
		}
		if info.IsDir() {
			continue
		}
		files = append(files, m)
	}
	return files, nil
}

func isGlob(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

// detectFormat sniffs a single file's format when -format/SECRETS_CONFIG_FORMAT
// is "auto": a ".json" extension wins outright, otherwise the first
// non-whitespace byte of the content decides ('{' => json, else shell).
func detectFormat(path string, data []byte) string {
	if strings.HasSuffix(path, ".json") {
		return "json"
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return "json"
	}
	return "shell"
}

// parseShell parses the subset of shell assignment syntax described in
// spec §5: an optional "export " prefix, KEY=VALUE, optional matching
// quotes around VALUE, blank lines and full-line "#" comments ignored.
// A line that is none of these (no "=") is a hard parse error.
func parseShell(path string, data []byte) (map[string]string, error) {
	out := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "export") && len(line) > len("export") {
			if c := line[len("export")]; c == ' ' || c == '\t' {
				line = strings.TrimSpace(line[len("export"):])
			}
		}

		idx := strings.Index(line, "=")
		if idx <= 0 {
			return nil, fmt.Errorf("%s:%d: malformed line (expected KEY=VALUE): %q", path, lineNo, raw)
		}

		key := line[:idx]
		value := unquote(line[idx+1:])
		out[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return out, nil
}

func unquote(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// parseJSON parses a flat top-level JSON object per spec §5. Numbers use
// json.Number so their original textual form is preserved exactly
// ("3" stays "3", never "3.0"). A nested object or array value is a hard
// parse error, never silently flattened.
func parseJSON(path string, data []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var raw interface{}
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}

	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s: top-level JSON value must be an object", path)
	}

	out := make(map[string]string, len(obj))
	for k, v := range obj {
		s, err := jsonScalarToString(v)
		if err != nil {
			return nil, fmt.Errorf("%s: key %q: %w", path, k, err)
		}
		out[k] = s
	}
	return out, nil
}

func jsonScalarToString(v interface{}) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		return t.String(), nil
	case string:
		return t, nil
	default:
		return "", fmt.Errorf("unsupported JSON value type %T (nested objects/arrays are not allowed)", v)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
