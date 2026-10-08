package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeFiles(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		data, ok := files[path]
		if !ok {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		}

		return []byte(data), nil
	}
}

func TestInjectReplacesPlaceholderKeepingItsIndentation(t *testing.T) {
	input := "    env:\n      # {{common}}\n"
	snippet := "      - name: A\n        value: \"1\"\n      - name: B\n        value: \"2\"\n"

	got, err := Inject(input, []Rule{{Variable: "common", File: "s.yaml"}}, 0, fakeFiles(map[string]string{"s.yaml": snippet}))
	if err != nil {
		t.Fatal(err)
	}

	want := "    env:\n      - name: A\n        value: \"1\"\n      - name: B\n        value: \"2\"\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestInjectIndentDelta(t *testing.T) {
	files := fakeFiles(map[string]string{"s.yaml": "  - a\n  - b\n"})

	tests := []struct {
		name  string
		delta int
		want  string
	}{
		// Matches the original image exactly: a positive delta also prefixes the FIRST line,
		// after its own leading whitespace was trimmed.
		{"positive", 2, "x: # pre\n  - a\n    - b\n"},
		{"negative", -2, "x: # pre\n- a\n- b\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Inject("x: # pre\n# {{v}}\n", []Rule{{Variable: "v", File: "s.yaml"}}, tt.delta, files)
			if err != nil {
				t.Fatal(err)
			}

			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInjectReplacesEveryOccurrenceAndAppliesRulesInOrder(t *testing.T) {
	files := fakeFiles(map[string]string{"a": "A", "b": "B"})
	rules := []Rule{{Variable: "a", File: "a"}, {Variable: "b", File: "b"}}

	got, err := Inject("# {{a}} # {{b}} # {{a}}", rules, 0, files)
	if err != nil {
		t.Fatal(err)
	}

	if got != "A B A" {
		t.Fatalf("got %q", got)
	}
}

func TestInjectLeavesUnmatchedPlaceholdersAndOtherTextAlone(t *testing.T) {
	input := "args: [\"sh\", \"-c\", \"echo $HOME ${X}\"]\n# {{other}}\n{{a}}\n"

	got, err := Inject(input, []Rule{{Variable: "a", File: "a"}}, 0, fakeFiles(map[string]string{"a": "A"}))
	if err != nil {
		t.Fatal(err)
	}

	if got != input {
		t.Fatalf("input was changed:\n%s", got)
	}
}

// The original image logged a missing snippet and substituted "", deploying a manifest without
// its environment variables. That must now fail.
func TestInjectMissingSnippetFails(t *testing.T) {
	_, err := Inject("# {{a}}", []Rule{{Variable: "a", File: "missing.yaml"}}, 0, fakeFiles(nil))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want a not-exist error, got %v", err)
	}
}

func TestParseConfigAcceptsBothIndentStyles(t *testing.T) {
	want := []Rule{
		{Variable: "common_environment_variables", File: "/app/common.yaml"},
		{Variable: "env_environment_variables", File: "/app/staging.yaml"},
	}

	for name, text := range map[string]string{
		"indented":             "---\nrules:\n  - variable: common_environment_variables\n    file: /app/common.yaml\n  - variable: env_environment_variables\n    file: /app/staging.yaml\n",
		"flush":                "---\nrules:\n- variable: common_environment_variables\n  file: /app/common.yaml\n- variable: env_environment_variables\n  file: /app/staging.yaml",
		"commented and quoted": "# rules for staging\nrules:\n  # first\n  - variable: \"common_environment_variables\" # inline\n    file: '/app/common.yaml'\n\n  - file: /app/staging.yaml\n    variable: env_environment_variables\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseConfig(text)
			if err != nil {
				t.Fatal(err)
			}

			if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseConfigRejectsWhatItDoesNotUnderstand(t *testing.T) {
	for name, text := range map[string]string{
		"empty":               "",
		"no rules":            "rules:\n",
		"other top-level key": "templates:\n  - variable: a\n    file: b\n",
		"unknown rule key":    "rules:\n  - variable: a\n    file: b\n    indent: 2\n",
		"missing file":        "rules:\n  - variable: a\n",
		"missing variable":    "rules:\n  - file: b\n",
		"duplicate key":       "rules:\n  - variable: a\n    variable: c\n    file: b\n",
		"flow style":          "rules: [{variable: a, file: b}]\n",
		"tab indentation":     "rules:\n\t- variable: a\n\t  file: b\n",
		"content before":      "  - variable: a\n    file: b\nrules:\n",
		"unterminated quote":  "rules:\n  - variable: \"a\n    file: b\n",
		"mixed list indent":   "rules:\n  - variable: a\n    file: b\n    - variable: c\n      file: d\n",
	} {
		t.Run(name, func(t *testing.T) {
			if rules, err := ParseConfig(text); err == nil {
				t.Fatalf("accepted %q as %+v", text, rules)
			}
		})
	}
}

func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	snippet := write("envs.yaml", "  - name: A\n    value: \"1\"\n")
	config := write("config.yaml", "rules:\n  - variable: envs\n    file: "+snippet+"\n")
	input := write("in.yaml", "env:\n  # {{envs}}\n")
	output := filepath.Join(dir, "out.yaml")

	if err := run([]string{config, input, output}, "2"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}

	if want := "env:\n    - name: A\n      value: \"1\"\n"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRunErrors(t *testing.T) {
	dir := t.TempDir()

	var usage usageError
	if err := run([]string{"a", "b"}, ""); !errors.As(err, &usage) {
		t.Fatalf("want usage error, got %v", err)
	}

	if err := run([]string{"a", "b", "c"}, "two"); err == nil || !strings.Contains(err.Error(), "INDENT_DELTA") {
		t.Fatalf("want INDENT_DELTA error, got %v", err)
	}

	if err := run([]string{filepath.Join(dir, "nope.yaml"), "b", "c"}, ""); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want missing config error, got %v", err)
	}

	if err := run([]string{dir, "b", "c"}, ""); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("want directory error, got %v", err)
	}
}
