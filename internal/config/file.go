package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultConfigPaths are searched when no config file is named explicitly.
//
// /config.ini is included because that is where the older GoGMC320 exporter's
// file was mounted, so an existing deployment keeps working after swapping the
// image rather than starting up with no configuration at all.
var DefaultConfigPaths = []string{
	"/etc/gmc-exporter/config.ini",
	"/config.ini",
}

// fileSettings is a parsed config file: setting names without the GOGMC_
// prefix, mapped to their values.
type fileSettings struct {
	// values maps a setting name to its value.
	values map[string]string
	// path is the file the values came from, for logging.
	path string
	// legacy records whether the file used the older exporter's schema.
	legacy bool
	// notes carries anything worth telling the operator about the parse,
	// such as a setting that no longer exists.
	notes []string
}

// LoadConfigFile reads an INI file into settings.
//
// Two schemas are recognised. The native one uses this exporter's own setting
// names, so a file is a direct transcription of the environment variables. The
// legacy one is the schema used by the older GoGMC320 exporter, recognised by
// its section names and translated automatically, so an existing config.ini
// keeps working unchanged.
func LoadConfigFile(path string) (*fileSettings, error) {
	f, err := os.Open(path) //nolint:gosec // the path is operator-supplied configuration
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	raw := map[string]map[string]string{}
	section := ""

	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			continue
		}

		if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
			section = strings.ToLower(strings.TrimSpace(text[1 : len(text)-1]))
			continue
		}

		key, value, found := strings.Cut(text, "=")
		if !found {
			return nil, fmt.Errorf("%s line %d: %q is not a key = value pair", path, line, text)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		// Values are sometimes quoted in hand-written files.
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' ||
			value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}

		if raw[section] == nil {
			raw[section] = map[string]string{}
		}
		raw[section][key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	if isLegacySchema(raw) {
		values, notes := translateLegacy(raw)
		return &fileSettings{values: values, path: path, legacy: true, notes: notes}, nil
	}
	return &fileSettings{values: translateNative(raw), path: path}, nil
}

// isLegacySchema reports whether the file uses the older exporter's sections.
func isLegacySchema(raw map[string]map[string]string) bool {
	_, hasRadmon := raw["radmon.org"]
	_, hasInflux := raw["influxdb"]
	return hasRadmon || hasInflux
}

// translateNative reads a file written against this exporter's own settings.
//
// Section names are used only as a prefix when the key does not already carry
// one, so both of these describe the same setting:
//
//	[radmon]
//	user = doug
//
//	RADMON_USER = doug
func translateNative(raw map[string]map[string]string) map[string]string {
	out := map[string]string{}
	for section, kv := range raw {
		prefix := strings.ToUpper(strings.ReplaceAll(section, ".", "_"))
		for key, value := range kv {
			name := strings.ToUpper(key)

			// A key written with the full GOGMC_ prefix is already
			// fully qualified. Prefixing it with the enclosing section
			// as well would silently rename it to something that does
			// not exist.
			if strings.HasPrefix(name, EnvPrefix) {
				out[strings.TrimPrefix(name, EnvPrefix)] = value
				continue
			}

			if prefix != "" && !strings.HasPrefix(name, prefix+"_") {
				name = prefix + "_" + name
			}
			out[name] = value
		}
	}
	return out
}

// translateLegacy maps the older GoGMC320 exporter's config.ini onto this
// exporter's settings.
//
// Presence of a section enables the corresponding sink, because in that file a
// section existing was how a backend was turned on; there was no enable flag.
// The InfluxDB defaults are set to the legacy measurement and field names so
// the series continues rather than a parallel one starting beside it.
func translateLegacy(raw map[string]map[string]string) (map[string]string, []string) {
	out := map[string]string{}
	var notes []string

	if kv, ok := raw["radmon.org"]; ok {
		out["RADMON_ENABLED"] = "true"
		if v, ok := kv["user"]; ok {
			out["RADMON_USER"] = v
		}
		if v, ok := kv["password"]; ok {
			out["RADMON_PASSWORD"] = v
		}
	}

	if kv, ok := raw["influxdb"]; ok {
		out["INFLUX1_ENABLED"] = "true"
		if v, ok := kv["url"]; ok {
			out["INFLUX1_URL"] = v
		}
		// The older file used "user"; some variants of it used "username".
		// Accepting both costs nothing and removes a silent misconfiguration.
		if v, ok := kv["user"]; ok {
			out["INFLUX1_USER"] = v
		}
		if v, ok := kv["username"]; ok {
			out["INFLUX1_USER"] = v
		}
		if v, ok := kv["password"]; ok {
			out["INFLUX1_PASSWORD"] = v
		}
		if v, ok := kv["database"]; ok {
			out["INFLUX1_DATABASE"] = v
		}

		// The older exporter wrote a measurement named "data" with its own
		// field names. Defaulting to those here is the point of recognising
		// the legacy schema: an upgrade continues the existing series instead
		// of silently starting a new one that no dashboard is querying.
		out["INFLUX1_MEASUREMENT"] = "data"
		out["INFLUX1_FIELD_STYLE"] = StyleLegacyName
		out["INFLUX1_TAG_DEVICE"] = "true"
	}

	if kv, ok := raw["main"]; ok {
		if v, ok := kv["poll"]; ok {
			if _, err := strconv.Atoi(v); err == nil {
				// The legacy file records a bare number of seconds.
				out["POLL_INTERVAL"] = v + "s"
			} else {
				out["POLL_INTERVAL"] = v
			}
		}
	}

	if _, ok := raw["watchdog"]; ok {
		notes = append(notes,
			"the [watchdog] section is ignored: this exporter cannot hang on a "+
				"read, and exposes /readyz and /health for external monitoring instead")
	}

	notes = append(notes,
		"recognised the legacy GoGMC320 config schema; InfluxDB defaults to "+
			"measurement \"data\" with legacy field names so the existing series continues")

	return out, notes
}

// StyleLegacyName is the field style name selecting the older exporter's
// InfluxDB field names. It is duplicated here rather than imported from the
// sink package to keep configuration free of dependencies on sinks.
const StyleLegacyName = "legacy"

// FindConfigFile returns the first of DefaultConfigPaths that exists, or an
// empty string. A path that cannot be read is reported rather than skipped,
// since silently ignoring a config file the operator mounted is how a
// deployment ends up running on defaults without anyone noticing.
func FindConfigFile() (string, error) {
	for _, path := range DefaultConfigPaths {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("checking for config file %s: %w", path, err)
		}
		if info.IsDir() {
			continue
		}
		return path, nil
	}
	return "", nil
}
