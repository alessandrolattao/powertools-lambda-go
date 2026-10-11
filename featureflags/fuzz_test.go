package featureflags

import (
	"encoding/json/v2"
	"errors"
	"testing"
	"time"
)

func FuzzValidate(f *testing.F) {
	for _, seed := range []string{
		`{}`, `[]`, `"x"`, `{"f":{"default":true}}`, `{"f":{"default":true,"rules":[]}}`,
		`{"f":{"default":false,"rules":{"r":{"when_match":true,"conditions":[{"action":"EQUALS","key":"k","value":1}]}}}}`,
		`{"f":{"default":false,"rules":{"r":{"when_match":true,"conditions":[{"action":"SCHEDULE_BETWEEN_DATETIME_RANGE","key":"CURRENT_DATETIME","value":{"START":"2022-01-01","END":"2022-01-02T10:00","TIMEZONE":"Europe/Rome"}}]}}}}`,
		`{"f":{"default":false,"rules":{"r":{"when_match":true,"conditions":[{"action":"MODULO_RANGE","key":"k","value":{"BASE":10,"START":0,"END":true}}]}}}}`,
		`{"a":1,"a":2}`, `{"f":`,
	} {
		f.Add([]byte(seed), []byte(`{"k":1}`))
	}
	f.Fuzz(func(t *testing.T, doc, evalCtx []byte) {
		_, err := Validate(doc)
		if err != nil {
			if !errors.Is(err, ErrSchemaValidation) {
				t.Fatalf("a validation failure is not a SchemaValidationError: %v", err)
			}
			return
		}
		// A valid schema evaluates against any context without panicking.
		var values map[string]any
		if err := json.Unmarshal(evalCtx, &values); err != nil {
			values = nil
		}
		flags := New(staticStore{raw: doc}, WithWarningHandler(func(string) {}), WithClock(func() time.Time { return time.Unix(0, 0) }))
		_, _ = flags.EnabledFeatures(t.Context(), values) //nolint:errcheck // only panics matter here
	})
}

func FuzzParseISODatetime(f *testing.F) {
	for _, seed := range []string{
		"2022-01-01", "2022-01-01T10", "2022-01-01T10:10:10.123456", "2022-01-01 10:10+01:00",
		"2022-02-29", "2022-01-01T24:00", "2022-01-01T10:10:10.1234567", "２０２２-01-01", "2022-01-01T10:10:10+01:00:30.5",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		wall, _, ok := parseISODatetime(s)
		if !ok {
			return
		}
		if wall.Location() != time.UTC || wall.Year() < 1 || wall.Year() > 9999 {
			t.Fatalf("%q parsed to %v", s, wall)
		}
		if got := wall.Format("2006-01-02"); got != s[:10] {
			t.Fatalf("%q parsed to the date %s", s, got)
		}
	})
}

func FuzzMatchTimeOfDay(f *testing.F) {
	for _, seed := range []string{"00:00", "23:59", "24:00", "1:00", "10:00\n", "１０:00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		hour, minute, ok := matchTimeOfDay(s)
		if ok && (hour > 23 || minute > 59 || hour < 0 || minute < 0) {
			t.Fatalf("%q matched as %d:%d", s, hour, minute)
		}
	})
}
