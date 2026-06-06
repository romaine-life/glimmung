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
