package portal

import "encoding/json"

// Op is one field predicate of a Where filter (§6 grammar). All set members
// must hold (AND semantics). Values are compared as JSON scalars: strings,
// numbers, booleans.
type Op struct {
	// Eq: the field equals any of these values (single-element slice for a
	// plain equality).
	Eq []any
	// Neq: the field equals none of these values.
	Neq []any
	// In: the field is one of these values.
	In []any
	// Gt/Lt: strict ordering; numbers and strings only.
	Gt any
	Lt any
}

// Where filters records by field name, ANDing all field predicates.
type Where map[string]Op

// normalize coerces integers to float64 so user-written literals compare
// equal to JSON-decoded numbers.
func normalize(value any) any {
	switch v := value.(type) {
	case int:
		return float64(v)
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	case float32:
		return float64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return string(v)
		}
		return f
	default:
		return value
	}
}

func scalarEqual(a, b any) bool {
	return normalize(a) == normalize(b)
}

func containsScalar(list []any, value any) bool {
	for _, candidate := range list {
		if scalarEqual(candidate, value) {
			return true
		}
	}
	return false
}

// orderedCompare applies cmp when both values are numbers or both are strings;
// any other pairing fails the predicate (mirroring the JS SDK).
func orderedCompare(value, bound any, cmp func(int) bool) bool {
	nv, nb := normalize(value), normalize(bound)
	if fv, ok := nv.(float64); ok {
		if fb, ok := nb.(float64); ok {
			switch {
			case fv < fb:
				return cmp(-1)
			case fv > fb:
				return cmp(1)
			default:
				return cmp(0)
			}
		}
		return false
	}
	if sv, ok := nv.(string); ok {
		if sb, ok := nb.(string); ok {
			switch {
			case sv < sb:
				return cmp(-1)
			case sv > sb:
				return cmp(1)
			default:
				return cmp(0)
			}
		}
	}
	return false
}

func matchesOp(value any, op Op) bool {
	if op.Eq != nil && !containsScalar(op.Eq, value) {
		return false
	}
	if op.Neq != nil && containsScalar(op.Neq, value) {
		return false
	}
	if op.In != nil && !containsScalar(op.In, value) {
		return false
	}
	if op.Gt != nil && !orderedCompare(value, op.Gt, func(c int) bool { return c > 0 }) {
		return false
	}
	if op.Lt != nil && !orderedCompare(value, op.Lt, func(c int) bool { return c < 0 }) {
		return false
	}
	return true
}

func matchesWhere(record map[string]any, where Where) bool {
	for field, op := range where {
		if !matchesOp(record[field], op) {
			return false
		}
	}
	return true
}
