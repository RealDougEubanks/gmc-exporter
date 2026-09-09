package influxv1

import "fmt"

// FieldNames is the set of field keys written to InfluxDB.
//
// These are configurable because an exporter replacing an existing one has to
// land in the same series, or every dashboard and alert built on the old data
// silently stops matching. Changing the field names is far cheaper than
// rewriting someone's Grafana.
type FieldNames struct {
	CPM           string
	AverageCPM    string
	MicroSieverts string
	Voltage       string
	Temperature   string
}

// Field naming styles.
const (
	// StyleSnake is the default: lower-case, underscore-separated keys that
	// match the Prometheus and MQTT sinks.
	StyleSnake = "snake"

	// StyleLegacy matches the field names used by the older, unmaintained
	// GoGMC320 exporter, so this one can continue an existing series instead
	// of starting a parallel one beside it.
	StyleLegacy = "legacy"
)

// snakeFields is the default naming.
var snakeFields = FieldNames{
	CPM:           "cpm",
	AverageCPM:    "acpm",
	MicroSieverts: "usvh",
	Voltage:       "volts",
	Temperature:   "temp_c",
}

// legacyFields reproduces the older exporter's schema exactly, including its
// capitalisation. The names were read from a live database rather than from
// that project's source.
var legacyFields = FieldNames{
	CPM:           "CPM",
	AverageCPM:    "ACPM",
	MicroSieverts: "USV",
	Voltage:       "Voltage",
	Temperature:   "Temperature",
}

// FieldNamesFor resolves a style name.
func FieldNamesFor(style string) (FieldNames, error) {
	switch style {
	case "", StyleSnake:
		return snakeFields, nil
	case StyleLegacy:
		return legacyFields, nil
	default:
		return FieldNames{}, fmt.Errorf(
			"unknown field style %q, expected %q or %q", style, StyleSnake, StyleLegacy)
	}
}
