// Command yaml-template-injector pastes shared YAML snippets into Kubernetes manifests.
//
// YAML has no include mechanism, so manifests mark where a shared block belongs with a comment
// placeholder, and this tool replaces each placeholder with the contents of a snippet file:
//
//	env:
//	  # {{common_environment_variables}}
//
// Usage:
//
//	yaml-template-injector CONFIG.yaml INPUT.yaml OUTPUT.yaml
//
// CONFIG lists the rules, one placeholder variable and one snippet file each:
//
//	rules:
//	  - variable: common_environment_variables
//	    file: .build/k8s/common/templates/environment-variables.template.yaml
//
// Relative snippet paths resolve against the working directory. INDENT_DELTA (an integer,
// default 0) shifts every line of a snippet right (positive) or left (negative) by that many
// spaces, for placeholders that sit at a different depth than the snippet was written for.
//
// It is a drop-in replacement for the gcr.io/.../yaml-template-injector image that Rebuy's Go
// services ran at deploy time, with the same arguments and output, and two deliberate
// differences: a snippet file that cannot be read is an error (the image logged it and
// substituted an empty string, so a manifest deployed without its environment variables), and a
// config with no rules is an error.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Getenv("INDENT_DELTA")); err != nil {
		fmt.Fprintf(os.Stderr, "yaml-template-injector: %v\n", err)

		var usage usageError
		if errors.As(err, &usage) {
			fmt.Fprintln(os.Stderr, "usage: yaml-template-injector CONFIG.yaml INPUT.yaml OUTPUT.yaml")
			os.Exit(2)
		}

		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// Rule maps a placeholder variable to the snippet file that replaces it.
type Rule struct {
	Variable string
	File     string
}

func run(args []string, indentDeltaEnv string) error {
	if len(args) != 3 {
		return usageError{fmt.Sprintf("expected 3 arguments, got %d", len(args))}
	}

	configFile, inputFile, outputFile := args[0], args[1], args[2]

	indentDelta := 0

	if indentDeltaEnv != "" {
		n, err := strconv.Atoi(indentDeltaEnv)
		if err != nil {
			return fmt.Errorf("INDENT_DELTA must be an integer, got %q", indentDeltaEnv)
		}

		indentDelta = n
	}

	configData, err := readRegularFile(configFile, "config")
	if err != nil {
		return err
	}

	rules, err := ParseConfig(string(configData))
	if err != nil {
		return fmt.Errorf("config %s: %w", configFile, err)
	}

	input, err := readRegularFile(inputFile, "input")
	if err != nil {
		return err
	}

	output, err := Inject(string(input), rules, indentDelta, os.ReadFile)
	if err != nil {
		return err
	}

	if err := os.WriteFile(outputFile, []byte(output), 0o644); err != nil { //nolint:gosec // manifests are not secret
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}

func readRegularFile(path, what string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s file: %w", what, err)
	}

	if info.IsDir() {
		return nil, fmt.Errorf("%s file is a directory: %s", what, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s file: %w", what, err)
	}

	return data, nil
}

// Inject replaces every "# {{variable}}" in input with the rule's snippet, rules applied in order.
//
// A snippet has one trailing newline removed, then its leading spaces and then its leading tabs
// (the placeholder's own indentation supplies the first line's), then is shifted by indentDelta.
// A placeholder that appears nowhere in input is not an error: one config serves many manifests.
func Inject(input string, rules []Rule, indentDelta int, readFile func(string) ([]byte, error)) (string, error) {
	snippets := make(map[string]string, len(rules))

	for _, rule := range rules {
		snippet, ok := snippets[rule.File]
		if !ok {
			data, err := readFile(rule.File)
			if err != nil {
				return "", fmt.Errorf("rule %q: read snippet: %w", rule.Variable, err)
			}

			snippet = strings.TrimSuffix(string(data), "\n")
			snippets[rule.File] = snippet
		}

		snippet = strings.TrimLeft(snippet, " ")
		snippet = strings.TrimLeft(snippet, "\t")

		if indentDelta != 0 {
			lines := strings.Split(snippet, "\n")
			for i := range lines {
				if indentDelta < 0 {
					lines[i] = strings.TrimPrefix(lines[i], strings.Repeat(" ", -indentDelta))
				} else {
					lines[i] = strings.Repeat(" ", indentDelta) + lines[i]
				}
			}

			snippet = strings.Join(lines, "\n")
		}

		input = strings.ReplaceAll(input, "# {{"+rule.Variable+"}}", snippet)
	}

	return input, nil
}

// ParseConfig reads the rules config. It accepts exactly the shape the tool documents, a
// top-level "rules:" sequence of mappings with "variable" and "file" keys, in either common
// indentation style, with an optional leading "---", comments, blank lines and quoted scalars.
// Anything else is an error rather than a guess: this module is standard-library-only, so this
// is a strict reader for one shape, not a YAML parser.
func ParseConfig(text string) ([]Rule, error) {
	var (
		rules   []Rule
		inRules bool
		cur     *Rule
		itemCol = -1
	)

	finish := func() error {
		if cur == nil {
			return nil
		}

		if cur.Variable == "" || cur.File == "" {
			return fmt.Errorf("rule %d: both variable and file are required", len(rules)+1)
		}

		rules = append(rules, *cur)
		cur = nil

		return nil
	}

	for n, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		lineNo := n + 1
		line := stripComment(raw)

		if strings.TrimSpace(line) == "" || (strings.TrimSpace(line) == "---" && !inRules) {
			continue
		}

		if strings.Contains(line, "\t") {
			return nil, fmt.Errorf("line %d: tabs are not valid YAML indentation", lineNo)
		}

		col := len(line) - len(strings.TrimLeft(line, " "))
		body := strings.TrimSpace(line)

		if col == 0 && !strings.HasPrefix(body, "- ") {
			if body != "rules:" {
				return nil, fmt.Errorf("line %d: unexpected top-level key %q (only \"rules:\" is supported)", lineNo, body)
			}

			if inRules {
				return nil, fmt.Errorf("line %d: duplicate \"rules:\"", lineNo)
			}

			inRules = true

			continue
		}

		if !inRules {
			return nil, fmt.Errorf("line %d: content before \"rules:\"", lineNo)
		}

		if strings.HasPrefix(body, "- ") || body == "-" {
			if itemCol == -1 {
				itemCol = col
			} else if col != itemCol {
				return nil, fmt.Errorf("line %d: inconsistent list indentation", lineNo)
			}

			if err := finish(); err != nil {
				return nil, err
			}

			cur = &Rule{}
			body = strings.TrimSpace(strings.TrimPrefix(body, "-"))

			if body == "" {
				continue
			}
		} else if cur == nil || col <= itemCol {
			return nil, fmt.Errorf("line %d: expected a \"- variable: ...\" list item", lineNo)
		}

		key, value, ok := strings.Cut(body, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", lineNo)
		}

		value, err := unquote(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}

		switch strings.TrimSpace(key) {
		case "variable":
			if cur.Variable != "" {
				return nil, fmt.Errorf("line %d: duplicate variable", lineNo)
			}

			cur.Variable = value
		case "file":
			if cur.File != "" {
				return nil, fmt.Errorf("line %d: duplicate file", lineNo)
			}

			cur.File = value
		default:
			return nil, fmt.Errorf("line %d: unknown key %q (only variable and file are supported)", lineNo, strings.TrimSpace(key))
		}
	}

	if err := finish(); err != nil {
		return nil, err
	}

	if len(rules) == 0 {
		return nil, errors.New("no rules")
	}

	return rules, nil
}

// stripComment drops a "#" comment that starts a line or follows whitespace, outside quotes.
func stripComment(line string) string {
	var quote rune

	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#' && (i == 0 || line[i-1] == ' '):
			return line[:i]
		}
	}

	return line
}

func unquote(v string) (string, error) {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if v[len(v)-1] != v[0] {
			return "", fmt.Errorf("unterminated quoted value %s", v)
		}

		if v[0] == '"' {
			s, err := strconv.Unquote(v)
			if err != nil {
				return "", fmt.Errorf("invalid quoted value %s: %w", v, err)
			}

			return s, nil
		}

		return strings.ReplaceAll(v[1:len(v)-1], "''", "'"), nil
	}

	if v == "" {
		return "", errors.New("empty value")
	}

	return v, nil
}
