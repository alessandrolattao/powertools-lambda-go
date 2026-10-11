package metrics

import (
	"fmt"
	"strings"
)

// Unit is the unit of measure of a CloudWatch metric. The constants are the
// units CloudWatch accepts; a Unit converted from a string may also carry the
// name of a Powertools Python MetricUnit member ("BytesPerSecond"), which
// AddMetric translates to the CloudWatch value ("Bytes/Second").
type Unit string

// The units CloudWatch accepts, as Powertools Python's MetricUnit lists them.
const (
	Seconds            Unit = "Seconds"
	Microseconds       Unit = "Microseconds"
	Milliseconds       Unit = "Milliseconds"
	Bytes              Unit = "Bytes"
	Kilobytes          Unit = "Kilobytes"
	Megabytes          Unit = "Megabytes"
	Gigabytes          Unit = "Gigabytes"
	Terabytes          Unit = "Terabytes"
	Bits               Unit = "Bits"
	Kilobits           Unit = "Kilobits"
	Megabits           Unit = "Megabits"
	Gigabits           Unit = "Gigabits"
	Terabits           Unit = "Terabits"
	Percent            Unit = "Percent"
	Count              Unit = "Count"
	BytesPerSecond     Unit = "Bytes/Second"
	KilobytesPerSecond Unit = "Kilobytes/Second"
	MegabytesPerSecond Unit = "Megabytes/Second"
	GigabytesPerSecond Unit = "Gigabytes/Second"
	TerabytesPerSecond Unit = "Terabytes/Second"
	BitsPerSecond      Unit = "Bits/Second"
	KilobitsPerSecond  Unit = "Kilobits/Second"
	MegabitsPerSecond  Unit = "Megabits/Second"
	GigabitsPerSecond  Unit = "Gigabits/Second"
	TerabitsPerSecond  Unit = "Terabits/Second"
	CountPerSecond     Unit = "Count/Second"
	NoUnit             Unit = "None"
)

// units pairs every unit with the name of its Python MetricUnit member, in
// the enum's order, which error messages follow.
var units = []struct {
	name string
	unit Unit
}{
	{"Seconds", Seconds},
	{"Microseconds", Microseconds},
	{"Milliseconds", Milliseconds},
	{"Bytes", Bytes},
	{"Kilobytes", Kilobytes},
	{"Megabytes", Megabytes},
	{"Gigabytes", Gigabytes},
	{"Terabytes", Terabytes},
	{"Bits", Bits},
	{"Kilobits", Kilobits},
	{"Megabits", Megabits},
	{"Gigabits", Gigabits},
	{"Terabits", Terabits},
	{"Percent", Percent},
	{"Count", Count},
	{"BytesPerSecond", BytesPerSecond},
	{"KilobytesPerSecond", KilobytesPerSecond},
	{"MegabytesPerSecond", MegabytesPerSecond},
	{"GigabytesPerSecond", GigabytesPerSecond},
	{"TerabytesPerSecond", TerabytesPerSecond},
	{"BitsPerSecond", BitsPerSecond},
	{"KilobitsPerSecond", KilobitsPerSecond},
	{"MegabitsPerSecond", MegabitsPerSecond},
	{"GigabitsPerSecond", GigabitsPerSecond},
	{"TerabitsPerSecond", TerabitsPerSecond},
	{"CountPerSecond", CountPerSecond},
	{"NoUnit", NoUnit},
}

// ParseUnit returns the unit s names: a CloudWatch unit ("Bytes/Second",
// "None") or the name of a Powertools Python MetricUnit member
// ("BytesPerSecond", "NoUnit"), matched exactly as Python matches them. Any
// other string is an ErrMetricUnit.
func ParseUnit(s string) (Unit, error) {
	for _, u := range units {
		if s == u.name {
			return u.unit, nil
		}
	}
	for _, u := range units {
		if Unit(s) == u.unit {
			return u.unit, nil
		}
	}
	names := make([]string, len(units))
	for i, u := range units {
		names[i] = u.name
	}
	return "", fmt.Errorf("%w %q, expected either option: %s", ErrMetricUnit, s, strings.Join(names, ", "))
}

// Resolution is the storage resolution of a CloudWatch metric.
type Resolution int

// The storage resolutions CloudWatch accepts. High resolution metrics keep
// one-second data points and cost more.
const (
	High     Resolution = 1
	Standard Resolution = 60
)

// check returns an ErrMetricResolution unless r is High or Standard.
func (r Resolution) check() error {
	if r != High && r != Standard {
		return fmt.Errorf("%w %d, expected either option: [60, 1]", ErrMetricResolution, r)
	}
	return nil
}
