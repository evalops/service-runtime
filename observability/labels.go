package observability

import "strings"

const otherLabelValue = "other"

// BoundedLabel clamps unexpected label values to "other" so metric series stay finite.
type BoundedLabel struct {
	name   string
	values map[string]struct{}
}

// NewBoundedLabel returns a label helper that only allows the provided values.
func NewBoundedLabel(name string, values ...string) BoundedLabel {
	allowedValues := make(map[string]struct{}, len(values)+1)
	allowedValues[otherLabelValue] = struct{}{}
	for _, value := range values {
		normalizedValue := strings.TrimSpace(value)
		if normalizedValue == "" {
			continue
		}
		allowedValues[normalizedValue] = struct{}{}
	}
	return BoundedLabel{
		name:   strings.TrimSpace(name),
		values: allowedValues,
	}
}

// Name returns the label name associated with the bounded value set.
func (label BoundedLabel) Name() string {
	return label.name
}

// Value returns the provided value when it is allowed, otherwise "other".
func (label BoundedLabel) Value(value string) string {
	normalizedValue := strings.TrimSpace(value)
	if normalizedValue == "" {
		return otherLabelValue
	}
	if _, ok := label.values[normalizedValue]; ok {
		return normalizedValue
	}
	return otherLabelValue
}
