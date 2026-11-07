package client

import (
	"fmt"
	"strconv"
)

// Typed reads over a JSON:API attributes map. A present-but-wrong-type
// key is API shape drift — an error, not a silent zero value. Absent
// and null both read as absent.

func shapeDrift(route, id string, err error) error {
	return fmt.Errorf("%s returned resource %s with a drifted attribute: %w", route, id, err)
}

// attrString reads a required string attribute.
func attrString(attrs map[string]any, key string) (string, error) {
	v, ok := attrs[key]
	if !ok || v == nil {
		return "", fmt.Errorf("%s is missing or null", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s is %T, want string", key, v)
	}
	return s, nil
}

// attrOptString reads an optional string attribute (nil when absent).
func attrOptString(attrs map[string]any, key string) (*string, error) {
	v, ok := attrs[key]
	if !ok || v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("%s is %T, want string", key, v)
	}
	return &s, nil
}

// attrOptBool reads an optional boolean attribute.
func attrOptBool(attrs map[string]any, key string) (*bool, error) {
	v, ok := attrs[key]
	if !ok || v == nil {
		return nil, nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("%s is %T, want bool", key, v)
	}
	return &b, nil
}

// attrOptInt64 reads an optional integer attribute (JSON numbers
// decode as float64).
func attrOptInt64(attrs map[string]any, key string) (*int64, error) {
	v, ok := attrs[key]
	if !ok || v == nil {
		return nil, nil
	}
	f, ok := v.(float64)
	if !ok {
		return nil, fmt.Errorf("%s is %T, want number", key, v)
	}
	i := int64(f)
	return &i, nil
}

// attrOptFloat64 reads an optional decimal attribute. The platform
// serializes :decimal as a JSON string ("0.5" — open_api.json), a bare
// number is accepted defensively.
func attrOptFloat64(attrs map[string]any, key string) (*float64, error) {
	v, ok := attrs[key]
	if !ok || v == nil {
		return nil, nil
	}
	switch f := v.(type) {
	case string:
		parsed, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is %q, not a decimal", key, f)
		}
		return &parsed, nil
	case float64:
		return &f, nil
	default:
		return nil, fmt.Errorf("%s is %T, want decimal string or number", key, v)
	}
}

// attrStringMap reads an optional string→string map attribute.
func attrStringMap(attrs map[string]any, key string) (map[string]string, error) {
	v, ok := attrs[key]
	if !ok || v == nil {
		return nil, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is %T, want object", key, v)
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		s, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%s] is %T, want string", key, k, val)
		}
		out[k] = s
	}
	return out, nil
}
