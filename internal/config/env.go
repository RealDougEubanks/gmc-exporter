package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/redact"
)

// EnvPrefix is prepended to every setting name.
const EnvPrefix = "GOGMC_"

// FileSuffix marks the variant of a setting that names a file to read the value
// from, rather than carrying the value itself.
//
// This matters for secrets. A plain environment variable is visible to anyone
// who can run `docker inspect`, and is inherited by every child process. Docker
// and Kubernetes secrets are delivered as files, so GOGMC_RADMON_PASSWORD_FILE
// is the correct way to supply a password to a container.
const FileSuffix = "_FILE"

// source records where a setting's value came from, for the startup log.
type source string

const (
	sourceDefault source = "default"
	sourceEnv     source = "env"
	sourceFile    source = "file"
)

// resolved is one setting's effective value and provenance.
type resolved struct {
	Key    string
	Source source
	// Display is the value as it should appear in a log: the real value for
	// ordinary settings, a placeholder for secrets.
	Display string
}

// loader reads settings from the environment, accumulating errors rather than
// failing on the first one.
//
// Reporting every configuration problem at once is deliberate: an operator
// fixing a container's environment should not have to restart it six times to
// discover six mistakes.
type loader struct {
	errs     []error
	resolved []resolved
	// lookupEnv is injectable so tests do not have to mutate the real
	// process environment.
	lookupEnv func(string) (string, bool)
	// readFile is injectable for the same reason.
	readFile func(string) ([]byte, error)
}

func newLoader() *loader {
	return &loader{
		lookupEnv: os.LookupEnv,
		readFile:  os.ReadFile,
	}
}

// raw returns the value for a setting, honouring the _FILE variant.
//
// Precedence is: NAME_FILE, then NAME, then the caller's default. Supplying
// both NAME and NAME_FILE is an error rather than a silent preference, because
// guessing which one the operator meant is how the wrong credential ends up in
// production.
func (l *loader) raw(name string) (string, source, bool) {
	key := EnvPrefix + name
	fileKey := key + FileSuffix

	fileVal, hasFile := l.lookupEnv(fileKey)
	envVal, hasEnv := l.lookupEnv(key)

	switch {
	case hasFile && hasEnv:
		l.errf("%s and %s are both set; supply exactly one", key, fileKey)
		return "", sourceDefault, false

	case hasFile:
		path := strings.TrimSpace(fileVal)
		if path == "" {
			l.errf("%s is set but empty", fileKey)
			return "", sourceDefault, false
		}
		content, err := l.readFile(path)
		if err != nil {
			// The path is safe to log; the contents are not.
			l.errf("%s: reading %s: %v", fileKey, path, err)
			return "", sourceDefault, false
		}
		// Trailing newlines are near-universal in secret files and are
		// almost never part of the credential.
		return strings.TrimRight(string(content), "\r\n"), sourceFile, true

	case hasEnv:
		return envVal, sourceEnv, true

	default:
		return "", sourceDefault, false
	}
}

// record notes a setting's effective value for the startup log.
func (l *loader) record(name string, src source, display string) {
	l.resolved = append(l.resolved, resolved{Key: EnvPrefix + name, Source: src, Display: display})
}

// String reads a plain string setting.
func (l *loader) String(name, def string) string {
	v, src, ok := l.raw(name)
	if !ok {
		l.record(name, sourceDefault, def)
		return def
	}
	l.record(name, src, v)
	return v
}

// Secret reads a credential. Its value is never recorded for logging.
func (l *loader) Secret(name string) redact.Secret {
	v, src, ok := l.raw(name)
	if !ok {
		l.record(name, sourceDefault, "(unset)")
		return redact.Secret{}
	}
	l.record(name, src, redact.Placeholder)
	return redact.New(v)
}

// Bool reads a boolean setting.
func (l *loader) Bool(name string, def bool) bool {
	v, src, ok := l.raw(name)
	if !ok {
		l.record(name, sourceDefault, strconv.FormatBool(def))
		return def
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		l.errf("%s%s: %q is not a boolean (use true or false)", EnvPrefix, name, v)
		return def
	}
	l.record(name, src, strconv.FormatBool(parsed))
	return parsed
}

// Int reads an integer setting and enforces an inclusive range.
func (l *loader) Int(name string, def, min, max int) int {
	v, src, ok := l.raw(name)
	if !ok {
		l.record(name, sourceDefault, strconv.Itoa(def))
		return def
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		l.errf("%s%s: %q is not a whole number", EnvPrefix, name, v)
		return def
	}
	if parsed < min || parsed > max {
		l.errf("%s%s: %d is outside the accepted range %d..%d", EnvPrefix, name, parsed, min, max)
		return def
	}
	l.record(name, src, strconv.Itoa(parsed))
	return parsed
}

// Duration reads a duration setting such as "60s" or "2m".
func (l *loader) Duration(name string, def, min, max time.Duration) time.Duration {
	v, src, ok := l.raw(name)
	if !ok {
		l.record(name, sourceDefault, def.String())
		return def
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		l.errf("%s%s: %q is not a duration (try 60s, 2m, 1h)", EnvPrefix, name, v)
		return def
	}
	if parsed < min || parsed > max {
		l.errf("%s%s: %s is outside the accepted range %s..%s", EnvPrefix, name, parsed, min, max)
		return def
	}
	l.record(name, src, parsed.String())
	return parsed
}

// Float reads a floating point setting and enforces an inclusive range.
func (l *loader) Float(name string, def, min, max float64) float64 {
	v, src, ok := l.raw(name)
	if !ok {
		l.record(name, sourceDefault, strconv.FormatFloat(def, 'g', -1, 64))
		return def
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		l.errf("%s%s: %q is not a number", EnvPrefix, name, v)
		return def
	}
	if parsed < min || parsed > max {
		l.errf("%s%s: %v is outside the accepted range %v..%v", EnvPrefix, name, parsed, min, max)
		return def
	}
	l.record(name, src, strconv.FormatFloat(parsed, 'g', -1, 64))
	return parsed
}

// OptionalFloat reads a float that may legitimately be absent, which is how
// latitude and longitude are distinguished from a deliberate zero.
func (l *loader) OptionalFloat(name string, min, max float64) (float64, bool) {
	v, src, ok := l.raw(name)
	if !ok || strings.TrimSpace(v) == "" {
		l.record(name, sourceDefault, "(unset)")
		return 0, false
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		l.errf("%s%s: %q is not a number", EnvPrefix, name, v)
		return 0, false
	}
	if parsed < min || parsed > max {
		l.errf("%s%s: %v is outside the accepted range %v..%v", EnvPrefix, name, parsed, min, max)
		return 0, false
	}
	l.record(name, src, strconv.FormatFloat(parsed, 'g', -1, 64))
	return parsed, true
}

// StringMap reads comma-separated key=value pairs, used for OTLP headers.
// Values are not logged, since headers commonly carry authorization tokens.
func (l *loader) StringMap(name string) map[string]string {
	v, src, ok := l.raw(name)
	if !ok || strings.TrimSpace(v) == "" {
		l.record(name, sourceDefault, "(unset)")
		return nil
	}

	out := map[string]string{}
	var keys []string
	for _, pair := range strings.Split(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, found := strings.Cut(pair, "=")
		if !found {
			l.errf("%s%s: %q is not in key=value form", EnvPrefix, name, pair)
			return nil
		}
		key = strings.TrimSpace(key)
		if key == "" {
			l.errf("%s%s: empty key in %q", EnvPrefix, name, pair)
			return nil
		}
		out[key] = strings.TrimSpace(value)
		keys = append(keys, key)
	}
	if len(out) == 0 {
		return nil
	}
	l.record(name, src, fmt.Sprintf("%d header(s): %s", len(out), strings.Join(keys, ", ")))
	return out
}

// Enum reads a setting constrained to a fixed set of values.
func (l *loader) Enum(name, def string, allowed ...string) string {
	v, src, ok := l.raw(name)
	if !ok {
		l.record(name, sourceDefault, def)
		return def
	}
	got := strings.ToLower(strings.TrimSpace(v))
	for _, a := range allowed {
		if got == a {
			l.record(name, src, got)
			return got
		}
	}
	l.errf("%s%s: %q is not one of %s", EnvPrefix, name, v, strings.Join(allowed, ", "))
	return def
}

// errf records a configuration problem.
func (l *loader) errf(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

// require reports a missing mandatory setting for an enabled sink.
func (l *loader) require(sinkName, settingName, value string) {
	if strings.TrimSpace(value) == "" {
		l.errf("%s is enabled but %s%s is not set", sinkName, EnvPrefix, settingName)
	}
}

// requireSecret is require for credentials, without touching the value.
func (l *loader) requireSecret(sinkName, settingName string, value redact.Secret) {
	if value.IsZero() {
		l.errf("%s is enabled but neither %s%s nor %s%s%s is set",
			sinkName, EnvPrefix, settingName, EnvPrefix, settingName, FileSuffix)
	}
}
