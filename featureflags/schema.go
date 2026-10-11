package featureflags

// The keys of the schema, as Python names them.
const (
	keyDefault     = "default"
	keyBooleanType = "boolean_type"
	keyRules       = "rules"
	keyWhenMatch   = "when_match"
	keyConditions  = "conditions"
	keyAction      = "action"
	keyKey         = "key"
	keyValue       = "value"
)

// RuleAction is the action of a condition: how the context value (the key)
// is compared with the condition value.
type RuleAction string

// The actions a condition can take. In the descriptions, key is the context
// value the condition names and value is the condition's value.
const (
	// Equals: key == value.
	Equals RuleAction = "EQUALS"
	// NotEquals: key != value.
	NotEquals RuleAction = "NOT_EQUALS"
	// KeyGreaterThanValue: key > value.
	KeyGreaterThanValue RuleAction = "KEY_GREATER_THAN_VALUE"
	// KeyGreaterThanOrEqualValue: key >= value.
	KeyGreaterThanOrEqualValue RuleAction = "KEY_GREATER_THAN_OR_EQUAL_VALUE"
	// KeyLessThanValue: key < value.
	KeyLessThanValue RuleAction = "KEY_LESS_THAN_VALUE"
	// KeyLessThanOrEqualValue: key <= value.
	KeyLessThanOrEqualValue RuleAction = "KEY_LESS_THAN_OR_EQUAL_VALUE"
	// StartsWith: the key string starts with the value string.
	StartsWith RuleAction = "STARTSWITH"
	// EndsWith: the key string ends with the value string.
	EndsWith RuleAction = "ENDSWITH"
	// In is KeyInValue under its older name.
	In RuleAction = "IN"
	// NotIn is KeyNotInValue under its older name.
	NotIn RuleAction = "NOT_IN"
	// KeyInValue: key is a member of the value list, a substring of the
	// value string, or a key of the value object.
	KeyInValue RuleAction = "KEY_IN_VALUE"
	// KeyNotInValue: the negation of KeyInValue.
	KeyNotInValue RuleAction = "KEY_NOT_IN_VALUE"
	// ValueInKey: value is a member of the key list, a substring of the key
	// string, or a key of the key object.
	ValueInKey RuleAction = "VALUE_IN_KEY"
	// ValueNotInKey: the negation of ValueInKey.
	ValueNotInKey RuleAction = "VALUE_NOT_IN_KEY"
	// AllInValue: every member of the key list is in the value list.
	AllInValue RuleAction = "ALL_IN_VALUE"
	// AnyInValue: at least one member of the key list is in the value list.
	AnyInValue RuleAction = "ANY_IN_VALUE"
	// NoneInValue: no member of the key list is in the value list.
	NoneInValue RuleAction = "NONE_IN_VALUE"
	// ScheduleBetweenTimeRange: the current time of day is between START and
	// END ("HH:MM", both included; a range whose end comes before its start
	// crosses midnight), in TIMEZONE (UTC by default). The key is
	// CURRENT_TIME.
	ScheduleBetweenTimeRange RuleAction = "SCHEDULE_BETWEEN_TIME_RANGE"
	// ScheduleBetweenDatetimeRange: the current date and time is between
	// START and END (ISO 8601 without offset, both included), in TIMEZONE.
	// The key is CURRENT_DATETIME.
	ScheduleBetweenDatetimeRange RuleAction = "SCHEDULE_BETWEEN_DATETIME_RANGE"
	// ScheduleBetweenDaysOfWeek: the current day of the week, in TIMEZONE,
	// is one of DAYS (MONDAY to SUNDAY). The key is CURRENT_DAY_OF_WEEK.
	ScheduleBetweenDaysOfWeek RuleAction = "SCHEDULE_BETWEEN_DAYS_OF_WEEK"
	// ModuloRange: START <= key % BASE <= END, for a numeric key: a stable
	// way to enable a feature for a share of users.
	ModuloRange RuleAction = "MODULO_RANGE"
)

// RuleActions lists every action, in the order Python's RuleAction declares
// them.
func RuleActions() []RuleAction {
	return []RuleAction{
		Equals, NotEquals,
		KeyGreaterThanValue, KeyGreaterThanOrEqualValue, KeyLessThanValue, KeyLessThanOrEqualValue,
		StartsWith, EndsWith,
		In, NotIn, KeyInValue, KeyNotInValue, ValueInKey, ValueNotInKey,
		AllInValue, AnyInValue, NoneInValue,
		ScheduleBetweenTimeRange, ScheduleBetweenDatetimeRange, ScheduleBetweenDaysOfWeek,
		ModuloRange,
	}
}

// The keys of time conditions, and of the members of their values.
const (
	CurrentTime      = "CURRENT_TIME"
	CurrentDatetime  = "CURRENT_DATETIME"
	CurrentDayOfWeek = "CURRENT_DAY_OF_WEEK"

	timeStart    = "START"
	timeEnd      = "END"
	timeTimezone = "TIMEZONE"
	timeDays     = "DAYS"

	moduloBase  = "BASE"
	moduloStart = "START"
	moduloEnd   = "END"
)

// The days of the week the schema accepts. Python's list also accepts
// "DAYS", a member of the same enum; it never matches a day.
var validDays = []string{"DAYS", "SUNDAY", "MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY"}
