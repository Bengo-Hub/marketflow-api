package custom_fields

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Errors the handler maps to 4xx answers.
var (
	ErrInvalid   = errors.New("invalid custom field")
	ErrDuplicate = errors.New("a field with this key already exists for this record type")
	ErrNotFound  = errors.New("custom field def not found")
)

var (
	entityTypes = map[string]bool{"lead": true, "contact": true, "deal": true}
	fieldTypes  = map[string]bool{"text": true, "number": true, "date": true, "select": true, "multiselect": true, "boolean": true}
	fieldKeyRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`)
)

const (
	maxLabelLen  = 80
	maxOptions   = 50
	maxOptionLen = 60
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// cleanOptions trims, drops blanks and repeats, and checks the limits.
func cleanOptions(in []string) ([]string, error) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, o := range in {
		o = strings.TrimSpace(o)
		if o == "" || seen[o] {
			continue
		}
		if utf8.RuneCountInString(o) > maxOptionLen {
			return nil, invalid("each choice must be %d characters or fewer", maxOptionLen)
		}
		seen[o] = true
		out = append(out, o)
	}
	if len(out) > maxOptions {
		return nil, invalid("at most %d choices", maxOptions)
	}
	return out, nil
}

func isChoice(fieldType string) bool { return fieldType == "select" || fieldType == "multiselect" }

// validateCreate normalises and checks a new definition.
func validateCreate(in *CreateDefInput) error {
	in.EntityType = strings.TrimSpace(in.EntityType)
	in.FieldKey = strings.TrimSpace(in.FieldKey)
	in.FieldLabel = strings.TrimSpace(in.FieldLabel)
	in.FieldType = strings.TrimSpace(in.FieldType)
	switch {
	case !entityTypes[in.EntityType]:
		return invalid("entity_type must be lead, contact or deal")
	case !fieldTypes[in.FieldType]:
		return invalid("field_type must be text, number, date, select, multiselect or boolean")
	case !fieldKeyRe.MatchString(in.FieldKey):
		return invalid("field_key must start with a letter and use only lowercase letters, digits and underscores (2 to 40 characters)")
	case in.FieldLabel == "" || utf8.RuneCountInString(in.FieldLabel) > maxLabelLen:
		return invalid("field_label is required and must be %d characters or fewer", maxLabelLen)
	}
	opts, err := cleanOptions(in.Options)
	if err != nil {
		return err
	}
	if isChoice(in.FieldType) && len(opts) == 0 {
		return invalid("a choice field needs at least one choice")
	}
	if !isChoice(in.FieldType) {
		opts = nil
	}
	in.Options = opts
	return nil
}
