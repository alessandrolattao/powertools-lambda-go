package featureflags

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Validate checks that doc is a valid feature flags schema, the way Python's
// SchemaValidator does, and returns the warnings Python would emit for it (a
// schema without features, a feature whose rules are empty). Use it to
// check a configuration before deploying it, in a test or a CI step.
//
// A failure is a *SchemaValidationError with Python's message, which names
// features and rules but never condition values. A document that is not
// JSON is a *SchemaValidationError too.
func Validate(doc []byte) (warnings []string, err error) {
	value, err := decodeJSON(doc)
	if err != nil {
		return nil, &SchemaValidationError{Message: "Feature flags schema is not valid JSON: " + err.Error()}
	}
	_, warnings, err = validateSchema(value)
	return warnings, err
}

// validateSchema is Python's SchemaValidator.validate on a decoded document.
func validateSchema(doc any) (features *object, warnings []string, err error) {
	features, ok := doc.(*object)
	if !ok {
		return nil, nil, schemaErrorf("Features must be a dictionary, schema=%s", pyStr(doc))
	}
	if features.len() == 0 {
		// Often an envelope that matched nothing: harmless, so a warning.
		return features, []string{"Feature flags schema is empty, no features to validate"}, nil
	}
	for _, name := range features.keys {
		feature := features.values[name]
		obj, booleanFeature, err := validateFeature(name, feature)
		if err != nil {
			return nil, warnings, err
		}
		warning, err := validateRules(name, obj, booleanFeature)
		if warning != "" {
			warnings = append(warnings, warning)
		}
		if err != nil {
			return nil, warnings, err
		}
	}
	return features, warnings, nil
}

// validateFeature checks one feature and reports whether it is a boolean
// one. As in Python, boolean_type counts by its truth value.
func validateFeature(name string, feature any) (obj *object, booleanFeature bool, err error) {
	obj, ok := feature.(*object)
	if !ok || obj.len() == 0 {
		return nil, false, schemaErrorf("Feature must be a non-empty dictionary, feature=%s", name)
	}
	booleanFeature = truthy(obj.getOr(keyBooleanType, true))
	def := obj.getOr(keyDefault, nil)
	if _, isBool := def.(bool); def == nil || !isBool && booleanFeature {
		return nil, false, schemaErrorf("feature 'default' boolean key must be present, feature=%s", name)
	}
	return obj, booleanFeature, nil
}

// validateRules checks the rules of a feature. Rules that are present but
// empty only warn: evaluation falls back to the default.
func validateRules(featureName string, feature *object, booleanFeature bool) (warning string, err error) {
	rules, present := feature.get(keyRules)
	if !truthy(rules) {
		if !present {
			return "", nil
		}
		if _, isObject := rules.(*object); isObject || rules == nil {
			return "Feature has 'rules' but it is empty, feature=" + featureName, nil
		}
		return fmt.Sprintf("Feature 'rules' should be a dictionary but is an empty %s, feature=%s", typeName(rules), featureName), nil
	}
	obj, ok := rules.(*object)
	if !ok {
		return "", schemaErrorf("Feature rules must be a dictionary, feature=%s", featureName)
	}
	for _, ruleName := range obj.keys {
		rule, err := validateRule(obj.values[ruleName], ruleName, featureName, booleanFeature)
		if err != nil {
			return "", err
		}
		if err := validateConditions(rule, ruleName); err != nil {
			return "", err
		}
	}
	return "", nil
}

// validateRule is Python's RulesValidator.validate_rule.
func validateRule(rule any, ruleName, featureName string, booleanFeature bool) (*object, error) {
	obj, ok := rule.(*object)
	if !ok || obj.len() == 0 {
		return nil, schemaErrorf("Feature rule must be a dictionary, feature=%s", featureName)
	}
	if err := validateRuleName(ruleName, featureName); err != nil {
		return nil, err
	}
	if _, isBool := obj.getOr(keyWhenMatch, nil).(bool); booleanFeature && !isBool {
		return nil, schemaErrorf("'rule_default_value' key must have be bool, rule=%s", ruleName)
	}
	return obj, nil
}

// validateRuleName is Python's RulesValidator.validate_rule_name. JSON
// names are strings, so only the empty one fails.
func validateRuleName(ruleName, featureName string) error {
	if ruleName == "" {
		return schemaErrorf("Rule name key must have a non-empty string, feature=%s", featureName)
	}
	return nil
}

// validateConditions is Python's ConditionsValidator.validate.
func validateConditions(rule *object, ruleName string) error {
	conditions, ok := rule.getOr(keyConditions, nil).([]any)
	if !ok || len(conditions) == 0 {
		return schemaErrorf("Invalid condition, rule=%s", ruleName)
	}
	for _, condition := range conditions {
		if err := validateCondition(ruleName, condition); err != nil {
			return err
		}
	}
	return nil
}

// validateCondition is Python's ConditionsValidator.validate_condition.
func validateCondition(ruleName string, condition any) error {
	obj, ok := condition.(*object)
	if !ok || obj.len() == 0 {
		return schemaErrorf("Feature rule condition must be a dictionary, rule=%s", ruleName)
	}
	if err := validateConditionAction(obj, ruleName); err != nil {
		return err
	}
	if err := validateConditionKey(obj, ruleName); err != nil {
		return err
	}
	return validateConditionValue(obj, ruleName)
}

// validateConditionAction checks that the action is one of RuleActions.
func validateConditionAction(condition *object, ruleName string) error {
	action := condition.getOr(keyAction, "")
	if name, ok := action.(string); ok && slices.Contains(RuleActions(), RuleAction(name)) {
		return nil
	}
	allowed := make([]string, 0, len(RuleActions()))
	for _, a := range RuleActions() {
		allowed = append(allowed, pyStringRepr(string(a)))
	}
	return schemaErrorf("'action' value must be either [%s], rule_name=%s, action=%s", strings.Join(allowed, ", "), ruleName, pyStr(action))
}

// validateConditionKey checks the key, and the key the time actions
// require.
func validateConditionKey(condition *object, ruleName string) error {
	key, ok := condition.getOr(keyKey, "").(string)
	if !ok || key == "" {
		return schemaErrorf("'key' value must be a non empty string, rule=%s", ruleName)
	}
	action, _ := condition.getOr(keyAction, "").(string) //nolint:errcheck // validateConditionAction checked it
	var want string
	switch RuleAction(action) {
	case ScheduleBetweenDaysOfWeek:
		want = CurrentDayOfWeek
	case ScheduleBetweenTimeRange:
		want = CurrentTime
	case ScheduleBetweenDatetimeRange:
		want = CurrentDatetime
	default:
		return nil
	}
	if key != want {
		return schemaErrorf("'condition with a '%s' action must have a '%s' condition key, rule=%s", action, want, ruleName)
	}
	return nil
}

// validateConditionValue checks that the value is present, and what each
// action requires of it.
func validateConditionValue(condition *object, ruleName string) error {
	value := condition.getOr(keyValue, nil)
	if value == nil {
		return schemaErrorf("'value' key must not be null, rule=%s", ruleName)
	}
	action, _ := condition.getOr(keyAction, "").(string) //nolint:errcheck // validateConditionAction checked it
	switch RuleAction(action) {
	case ScheduleBetweenDaysOfWeek:
		return validateDaysOfWeek(value, ruleName)
	case ScheduleBetweenTimeRange:
		return validateTimeRange(value, ruleName)
	case ScheduleBetweenDatetimeRange:
		return validateDatetimeRange(value, ruleName)
	case ModuloRange:
		return validateModuloRange(value, ruleName)
	case AllInValue, AnyInValue, NoneInValue:
		if _, ok := value.([]any); !ok {
			return schemaErrorf("%s action must have a list value, rule=%s", action, ruleName)
		}
	default:
	}
	return nil
}

func validateDaysOfWeek(value any, ruleName string) error {
	obj, ok := value.(*object)
	if !ok {
		return schemaErrorf("condition with a CURRENT_DAY_OF_WEEK action must have a condition value dictionary with 'DAYS' and 'TIMEZONE' (optional) keys, rule=%s", ruleName)
	}
	days, ok := obj.getOr(timeDays, nil).([]any)
	if !ok || obj.len() == 0 {
		return schemaErrorf("condition with a CURRENT_DAY_OF_WEEK action must have a condition value dictionary with 'DAYS' and 'TIMEZONE' (optional) keys, rule=%s", ruleName)
	}
	for _, day := range days {
		if name, ok := day.(string); !ok || !slices.Contains(validDays, name) {
			return schemaErrorf("condition value DAYS must represent a day of the week in 'TimeValues' enum, rule=%s", ruleName)
		}
	}
	return validateTimezone(obj.getOr(timeTimezone, nil), ruleName)
}

// startEnd returns START and END of a time condition, which must be strings
// ("" when absent, as Python's dict.get default).
func startEnd(obj *object, ruleName string) (start, end string, err error) {
	start, okStart := obj.getOr(timeStart, "").(string)
	end, okEnd := obj.getOr(timeEnd, "").(string)
	if !okStart || !okEnd {
		return "", "", schemaErrorf("'START' and 'END' must be a non empty string, rule=%s", ruleName)
	}
	return start, end, nil
}

func validateTimeRange(value any, ruleName string) error {
	obj, ok := value.(*object)
	if !ok {
		return schemaErrorf("%s action must have a dictionary with 'START' and 'END' keys, rule=%s", ScheduleBetweenTimeRange, ruleName)
	}
	start, end, err := startEnd(obj, ruleName)
	if err != nil {
		return err
	}
	_, _, okStart := matchTimeOfDay(start)
	_, _, okEnd := matchTimeOfDay(end)
	if !okStart || !okEnd {
		return schemaErrorf("'START' and 'END' must be a valid time format, time_format=%%H:%%M, rule=%s", ruleName)
	}
	return validateTimezone(obj.getOr(timeTimezone, nil), ruleName)
}

func validateDatetimeRange(value any, ruleName string) error {
	obj, ok := value.(*object)
	if !ok {
		return schemaErrorf("%s action must have a dictionary with 'START' and 'END' keys, rule=%s", ScheduleBetweenDatetimeRange, ruleName)
	}
	start, end, err := startEnd(obj, ruleName)
	if err != nil {
		return err
	}
	for _, s := range []string{start, end} {
		// Python replaces Z with +00:00 first, so that a Z is reported as
		// timezone information rather than as a malformed date.
		_, hasOffset, ok := parseISODatetime(strings.ReplaceAll(s, "Z", "+00:00"))
		if !ok {
			return schemaErrorf("'START' and 'END' must be a valid ISO8601 time format, rule=%s", ruleName)
		}
		if hasOffset {
			return schemaErrorf("'START' and 'END' must not include timezone information. Set the timezone using the 'TIMEZONE' field, rule=%s ", ruleName)
		}
	}
	return validateTimezone(obj.getOr(timeTimezone, nil), ruleName)
}

func validateModuloRange(value any, ruleName string) error {
	obj, ok := value.(*object)
	if !ok {
		return schemaErrorf("condition with a 'MODULO_RANGE' action must have a condition value type dictionary with 'BASE', 'START' and 'END' keys, rule=%s", ruleName)
	}
	base, start, end := obj.getOr(moduloBase, nil), obj.getOr(moduloStart, nil), obj.getOr(moduloEnd, nil)
	if base == nil || start == nil || end == nil {
		return schemaErrorf("condition with a 'MODULO_RANGE' action must have a condition value type dictionary with 'BASE', 'START' and 'END' keys, rule=%s", ruleName)
	}
	b, okB := pyInt(base)
	s, okS := pyInt(start)
	e, okE := pyInt(end)
	if !okB || !okS || !okE {
		return schemaErrorf("'BASE', 'START' and 'END' must be integers, rule=%s", ruleName)
	}
	if s < 0 || s > e || e > b-1 {
		return schemaErrorf("condition with 'MODULO_RANGE' action must satisfy 0 <= START <= END <= BASE-1, rule=%s", ruleName)
	}
	return nil
}

// pyInt reports whether v is a Python int, which a bool is, and its value.
func pyInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// validateTimezone checks TIMEZONE: absent or empty means UTC, anything else
// must name a zone of the IANA database. Python's dateutil also accepts
// POSIX TZ strings ("EST5EDT", "UTC+3"); Go reads IANA names only.
func validateTimezone(timezone any, ruleName string) error {
	if !truthy(timezone) {
		return nil
	}
	name, ok := timezone.(string)
	if !ok {
		// Python formats the builtin str here instead of the rule name.
		return schemaErrorf("'TIMEZONE' must be a string, rule=%s", ruleName)
	}
	if _, err := loadLocation(name); err != nil {
		return schemaErrorf("'TIMEZONE' value must represent a valid IANA timezone, rule=%s", ruleName)
	}
	return nil
}

// loadLocation loads an IANA zone from the embedded database. "UTC" and
// zone names are accepted; "Local" is refused, as it names no zone.
func loadLocation(name string) (*time.Location, error) {
	if name == "Local" {
		return nil, fmt.Errorf("%w: %q", errUnknownZone, name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnknownZone, err)
	}
	return loc, nil
}
