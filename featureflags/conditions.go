package featureflags

import (
	"errors"
	"strings"
	"time"
	_ "time/tzdata" // Lambda's OS images do not promise a zone database; time conditions need one.
)

// errUnknownZone reports a TIMEZONE that names no zone.
var errUnknownZone = errors.New("unknown time zone")

// compare runs the comparator of action on the context value a and the
// condition value b, as Python's RULE_ACTION_MAPPING does. An error stands
// for the exception Python would raise. An unknown action never matches.
func compare(action RuleAction, a, b any, now time.Time) (bool, error) {
	if comparator, ok := comparators[action]; ok {
		return comparator(a, b, now)
	}
	return false, nil
}

// comparator compares the context value a with the condition value b.
type comparator func(a, b any, now time.Time) (bool, error)

// comparators is Python's RULE_ACTION_MAPPING.
var comparators = map[RuleAction]comparator{
	Equals:                     func(a, b any, _ time.Time) (bool, error) { return pyEqual(a, b), nil },
	NotEquals:                  func(a, b any, _ time.Time) (bool, error) { return !pyEqual(a, b), nil },
	KeyGreaterThanValue:        func(a, b any, _ time.Time) (bool, error) { return pyOrder(a, b, opGreater) },
	KeyGreaterThanOrEqualValue: func(a, b any, _ time.Time) (bool, error) { return pyOrder(a, b, opGreaterEqual) },
	KeyLessThanValue:           func(a, b any, _ time.Time) (bool, error) { return pyOrder(a, b, opLess) },
	KeyLessThanOrEqualValue:    func(a, b any, _ time.Time) (bool, error) { return pyOrder(a, b, opLessEqual) },
	StartsWith:                 func(a, b any, _ time.Time) (bool, error) { return pyStartsWith(a, b, "startswith") },
	EndsWith:                   func(a, b any, _ time.Time) (bool, error) { return pyStartsWith(a, b, "endswith") },
	In:                         keyInValue,
	NotIn:                      keyNotInValue,
	KeyInValue:                 keyInValue,
	KeyNotInValue:              keyNotInValue,
	ValueInKey:                 func(a, b any, _ time.Time) (bool, error) { return pyContains(a, b) },
	ValueNotInKey: func(a, b any, _ time.Time) (bool, error) {
		in, err := pyContains(a, b)
		return !in, err
	},
	AnyInValue:                   func(a, b any, _ time.Time) (bool, error) { return compareList(AnyInValue, a, b) },
	AllInValue:                   func(a, b any, _ time.Time) (bool, error) { return compareList(AllInValue, a, b) },
	NoneInValue:                  func(a, b any, _ time.Time) (bool, error) { return compareList(NoneInValue, a, b) },
	ScheduleBetweenTimeRange:     func(_, b any, now time.Time) (bool, error) { return compareTimeRange(b, now) },
	ScheduleBetweenDatetimeRange: func(_, b any, now time.Time) (bool, error) { return compareDatetimeRange(b, now) },
	ScheduleBetweenDaysOfWeek:    func(_, b any, now time.Time) (bool, error) { return compareDaysOfWeek(b, now) },
	ModuloRange:                  func(a, b any, _ time.Time) (bool, error) { return compareModuloRange(a, b) },
}

func keyInValue(a, b any, _ time.Time) (bool, error) { return pyContains(b, a) }

func keyNotInValue(a, b any, _ time.Time) (bool, error) {
	in, err := pyContains(b, a)
	return !in, err
}

// compareList is ANY_IN_VALUE, ALL_IN_VALUE and NONE_IN_VALUE: the context
// value must be a list, each of its members is looked up in the condition
// value.
func compareList(action RuleAction, a, b any) (bool, error) {
	list, ok := a.([]any)
	if !ok {
		return false, valueErrorf("Context provided must be a list. Unable to compare %s action.", action)
	}
	for _, member := range list {
		in, err := pyContains(b, member)
		if err != nil {
			return false, err
		}
		switch {
		case action == AnyInValue && in:
			return true, nil
		case action == AllInValue && !in:
			return false, nil
		case action == NoneInValue && in:
			return false, nil
		}
	}
	return action != AnyInValue, nil
}

// conditionValue returns the member name of a time or modulo condition
// value, or fallback, as Python's dict.get.
func conditionValue(value any, name string, fallback any) any {
	if obj, ok := value.(*object); ok {
		return obj.getOr(name, fallback)
	}
	return fallback
}

// nowIn returns the wall clock of now in the condition's TIMEZONE (UTC when
// absent or empty, which is also the local zone of Lambda).
func nowIn(value any, now time.Time) (time.Time, error) {
	name, _ := conditionValue(value, timeTimezone, "UTC").(string) //nolint:errcheck // validation checked the type
	if name == "" {
		name = "UTC"
	}
	loc, err := loadLocation(name)
	if err != nil {
		return time.Time{}, typeErrorf("%v", err)
	}
	return now.In(loc), nil
}

// compareTimeRange is SCHEDULE_BETWEEN_TIME_RANGE: the time of day, to the
// minute, between START and END, both included. An END earlier than START
// crosses midnight.
func compareTimeRange(value any, now time.Time) (bool, error) {
	local, err := nowIn(value, now)
	if err != nil {
		return false, err
	}
	start, _ := conditionValue(value, timeStart, "").(string) //nolint:errcheck // validation checked the type
	end, _ := conditionValue(value, timeEnd, "").(string)     //nolint:errcheck // validation checked the type
	startHour, startMinute, okStart := matchTimeOfDay(start)
	endHour, endMinute, okEnd := matchTimeOfDay(end)
	if !okStart || !okEnd {
		return false, valueErrorf("invalid time range %s-%s", start, end)
	}
	// Python sets hour and minute on the current time and keeps its
	// seconds, so comparing minutes since midnight is the same.
	current := local.Hour()*60 + local.Minute()
	from, to := startHour*60+startMinute, endHour*60+endMinute
	if to < from {
		return current >= from || current <= to, nil
	}
	return from <= current && current <= to, nil
}

// compareDatetimeRange is SCHEDULE_BETWEEN_DATETIME_RANGE: START <= now <=
// END, all three on the wall clock of TIMEZONE, to the microsecond, as
// Python compares datetimes that share a tzinfo.
func compareDatetimeRange(value any, now time.Time) (bool, error) {
	local, err := nowIn(value, now)
	if err != nil {
		return false, err
	}
	wall := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond()/1000*1000, time.UTC)
	start, _ := conditionValue(value, timeStart, "").(string) //nolint:errcheck // validation checked the type
	end, _ := conditionValue(value, timeEnd, "").(string)     //nolint:errcheck // validation checked the type
	from, _, okStart := parseISODatetime(start)
	to, _, okEnd := parseISODatetime(end)
	if !okStart || !okEnd {
		return false, valueErrorf("Invalid isoformat string: %s", pyStringRepr(start))
	}
	return !wall.Before(from) && !wall.After(to), nil
}

// compareDaysOfWeek is SCHEDULE_BETWEEN_DAYS_OF_WEEK: the English name of
// the current day in TIMEZONE, upper case, is one of DAYS.
func compareDaysOfWeek(value any, now time.Time) (bool, error) {
	local, err := nowIn(value, now)
	if err != nil {
		return false, err
	}
	days, _ := conditionValue(value, timeDays, []any{}).([]any) //nolint:errcheck // validation checked the type
	return pyContains(days, strings.ToUpper(local.Weekday().String()))
}

// compareModuloRange is MODULO_RANGE: START <= key % BASE <= END, with
// Python's floored modulo.
func compareModuloRange(a, value any) (bool, error) {
	base, _ := pyInt(conditionValue(value, moduloBase, int64(1)))
	start := conditionValue(value, moduloStart, int64(1))
	end := conditionValue(value, moduloEnd, int64(1))
	if base == 0 {
		return false, typeErrorf("integer modulo by zero")
	}
	rest, err := pyModulo(a, base)
	if err != nil {
		return false, err
	}
	low, err := pyOrder(start, rest, opLessEqual)
	if err != nil || !low {
		return false, err
	}
	return pyOrder(rest, end, opLessEqual)
}
