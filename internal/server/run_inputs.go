package server

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	maxRunInputCount       = 32
	maxRunInputValueLength = 512
)

var (
	runInputNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	runInputRefPattern  = regexp.MustCompile(`^\s*\$\{\{\s*inputs\.([A-Za-z_][A-Za-z0-9_-]*)\s*\}\}\s*$`)
)

func normalizeRunInputs(inputs map[string]string) (map[string]string, error) {
	if len(inputs) == 0 {
		return map[string]string{}, nil
	}
	if len(inputs) > maxRunInputCount {
		return nil, fmt.Errorf("inputs has %d entries; maximum is %d", len(inputs), maxRunInputCount)
	}
	out := make(map[string]string, len(inputs))
	for rawKey, rawValue := range inputs {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			return nil, fmt.Errorf("inputs contains an empty key")
		}
		if !runInputNamePattern.MatchString(key) {
			return nil, fmt.Errorf("inputs key %q is invalid; use letters, numbers, underscores, or hyphens, starting with a letter or underscore", rawKey)
		}
		value := strings.TrimSpace(rawValue)
		if len(value) > maxRunInputValueLength {
			return nil, fmt.Errorf("inputs.%s is %d bytes; maximum is %d", key, len(value), maxRunInputValueLength)
		}
		out[key] = value
	}
	return out, nil
}

func runInputsForMetadata(inputs map[string]string) map[string]string {
	if len(inputs) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(inputs))
	for key, value := range inputs {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = strings.TrimSpace(value)
	}
	return out
}

func resolveRunInputTemplate(value string, inputs map[string]string) (string, error) {
	matches := runInputRefPattern.FindStringSubmatch(value)
	if matches == nil {
		return value, nil
	}
	key := matches[1]
	resolved, ok := inputs[key]
	if !ok {
		return "", fmt.Errorf("input %q is required by %q but was not provided", key, value)
	}
	return resolved, nil
}

func sortedRunInputKeys(inputs map[string]string) []string {
	keys := make([]string, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// resolveDispatchRunInputs reconciles the caller-supplied run inputs against
// the workflow's declared DispatchInputs. The returned map contains exactly
// the declared inputs, populated from the caller's values where present and
// from declared defaults otherwise.
//
// The rules are intentionally strict and symmetrical with the validator:
//   - A required input that the caller did not supply and that carries no
//     non-empty default is rejected. There is no server-side guess (no
//     "default to main", no "fall back to the project's branch") — the
//     migration policy forbids fallback defaults, so the dispatcher must
//     either declare a Default on the workflow input or supply the value.
//   - An undeclared input the caller supplied is rejected. The workflow's
//     dispatch_inputs is the contract; sending a key the workflow does not
//     reference would silently flow into Run.RunInputs and be untraceable.
//
// On success the returned map carries only declared keys, with any caller
// overrides applied on top of declared defaults.
func resolveDispatchRunInputs(declared []DispatchInputSpec, supplied map[string]string) (map[string]string, error) {
	declaredByName := make(map[string]DispatchInputSpec, len(declared))
	for _, input := range declared {
		declaredByName[input.Name] = input
	}
	for _, key := range sortedRunInputKeys(supplied) {
		if _, ok := declaredByName[key]; !ok {
			return nil, fmt.Errorf("inputs.%s is not declared by the workflow's dispatch_inputs; add it to dispatch_inputs or remove it from the dispatch payload", key)
		}
	}
	resolved := make(map[string]string, len(declared))
	for _, input := range declared {
		value, hasValue := supplied[input.Name]
		if hasValue && value != "" {
			resolved[input.Name] = value
			continue
		}
		if input.Default != "" {
			resolved[input.Name] = input.Default
			continue
		}
		if input.Required {
			return nil, fmt.Errorf("inputs.%s is required by the workflow but was not provided and has no default", input.Name)
		}
		// Non-required with empty default is rejected at register time, so
		// reaching here would mean a malformed registration slipped through;
		// be loud rather than persist an empty substitution.
		return nil, fmt.Errorf("inputs.%s is declared without a default and is not required; reject as malformed", input.Name)
	}
	return resolved, nil
}
