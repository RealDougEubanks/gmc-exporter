package mqtt

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/redact"
)

// canaryPassword is deliberately unlike anything else in this package, so a
// substring search for it in captured log output cannot match by accident.
const canaryPassword = "Zq7-CANARY-mqtt-password-3f9x"

// --- fakes -----------------------------------------------------------------

// fakeToken satisfies paho.Token without any network involvement. It is always
// already complete, since the fake client does its work synchronously.
type fakeToken struct {
	err  error
	done chan struct{}
}

func newFakeToken(err error) *fakeToken {
	done := make(chan struct{})
	close(done)
	return &fakeToken{err: err, done: done}
}

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return true }
func (t *fakeToken) Done() <-chan struct{}          { return t.done }
func (t *fakeToken) Error() error                   { return t.err }

// pendingToken never completes, so a test can exercise the context deadline
// path without waiting on a real broker.
type pendingToken struct{ done chan struct{} }

func (t *pendingToken) Wait() bool                     { return false }
func (t *pendingToken) WaitTimeout(time.Duration) bool { return false }
func (t *pendingToken) Done() <-chan struct{}          { return t.done }
func (t *pendingToken) Error() error                   { return nil }

// published records one call to Publish.
type published struct {
	topic    string
	qos      byte
	retained bool
	payload  string
}

// fakeClient records publishes instead of sending them.
type fakeClient struct {
	mu         sync.Mutex
	connected  bool
	failWith   error
	failTopic  string
	stall      bool
	messages   []published
	disconnect int
}

func (c *fakeClient) Publish(topic string, qos byte, retained bool, payload any) paho.Token {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stall {
		return &pendingToken{done: make(chan struct{})}
	}

	body, _ := payload.(string)
	c.messages = append(c.messages, published{topic: topic, qos: qos, retained: retained, payload: body})

	if c.failWith != nil && (c.failTopic == "" || c.failTopic == topic) {
		return newFakeToken(c.failWith)
	}
	return newFakeToken(nil)
}

func (c *fakeClient) IsConnectionOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *fakeClient) Disconnect(uint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnect++
	c.connected = false
}

func (c *fakeClient) sent() []published {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]published, len(c.messages))
	copy(out, c.messages)
	return out
}

func (c *fakeClient) payloadFor(topic string) (string, bool) {
	for _, m := range c.sent() {
		if m.topic == topic {
			return m.payload, true
		}
	}
	return "", false
}

// syncBuffer makes a bytes.Buffer safe for the paho callbacks and the test
// goroutine to write concurrently, so -race stays quiet.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// --- helpers ---------------------------------------------------------------

// discardLogger is for the tests that exercise a path whose log output they do
// not assert on.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testConfig() config.MQTT {
	return config.MQTT{
		Enabled:     true,
		Broker:      "broker.example.invalid",
		Port:        1883,
		Username:    "gmc",
		Password:    redact.New(canaryPassword),
		ClientID:    "gmc-exporter",
		QoS:         1,
		BaseTopic:   "gmc-exporter",
		Retain:      true,
		HADiscovery: true,
		HAPrefix:    "homeassistant",
		DeviceName:  "Geiger Counter",
		Timeout:     2 * time.Second,
	}
}

func testDevice() Device {
	return Device{Model: "GMC-320", Firmware: "Re 3.03", Serial: "a1b2c3"}
}

// newTestSink builds a Sink wired to a fake client, which is the whole point of
// keeping paho behind the publisher interface.
func newTestSink(t *testing.T, cfg config.MQTT) (*Sink, *fakeClient) {
	t.Helper()

	logger := slog.New(slog.NewJSONHandler(&syncBuffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := newSink(cfg, testDevice(), logger)
	if err != nil {
		t.Fatalf("newSink: %v", err)
	}
	client := &fakeClient{connected: true}
	s.client = client
	return s, client
}

func fullReading() reading.Reading {
	return reading.Reading{
		Timestamp:            time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
		CPM:                  42,
		AverageCPM:           38.456,
		MicroSievertsPerHour: 0.27311,
		Voltage:              reading.Some(4.85),
		TemperatureC:         reading.Some(21.44),
	}
}

// --- state publishing ------------------------------------------------------

func TestPublish_fullReading_writesEveryStateTopic(t *testing.T) {
	s, client := newTestSink(t, testConfig())

	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	want := []published{
		{topic: "gmc-exporter/cpm", qos: 1, retained: true, payload: "42"},
		{topic: "gmc-exporter/average_cpm", qos: 1, retained: true, payload: "38.46"},
		{topic: "gmc-exporter/usvh", qos: 1, retained: true, payload: "0.2731"},
		{topic: "gmc-exporter/voltage", qos: 1, retained: true, payload: "4.85"},
		{topic: "gmc-exporter/temperature", qos: 1, retained: true, payload: "21.4"},
	}
	got := client.sent()
	if len(got) != len(want) {
		t.Fatalf("published %d messages, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("message %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestPublish_invalidOptionals_omitsVoltageAndTemperature(t *testing.T) {
	s, client := newTestSink(t, testConfig())

	r := fullReading()
	r.Voltage = reading.None[float64]()
	r.TemperatureC = reading.None[float64]()

	if err := s.Publish(context.Background(), r); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for _, topic := range []string{"gmc-exporter/voltage", "gmc-exporter/temperature"} {
		if payload, ok := client.payloadFor(topic); ok {
			// Publishing zero here would be indistinguishable from a real
			// reading of zero, which is the bug this guards against.
			t.Errorf("published %q to %s for an absent value", payload, topic)
		}
	}
	if len(client.sent()) != 3 {
		t.Errorf("published %d messages, want 3", len(client.sent()))
	}
}

func TestPublish_zeroValuedOptionals_arePublished(t *testing.T) {
	s, client := newTestSink(t, testConfig())

	r := fullReading()
	r.CPM = 0
	r.Voltage = reading.Some(0.0)

	if err := s.Publish(context.Background(), r); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if payload, ok := client.payloadFor("gmc-exporter/voltage"); !ok || payload != "0.00" {
		t.Errorf("voltage payload = %q (present %v), want %q", payload, ok, "0.00")
	}
	if payload, ok := client.payloadFor("gmc-exporter/cpm"); !ok || payload != "0" {
		t.Errorf("cpm payload = %q (present %v), want %q", payload, ok, "0")
	}
}

func TestPublish_qosAndRetainFollowConfig(t *testing.T) {
	cfg := testConfig()
	cfg.QoS = 0
	cfg.Retain = false

	s, client := newTestSink(t, cfg)
	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for _, m := range client.sent() {
		if m.qos != 0 {
			t.Errorf("%s published with qos %d, want 0", m.topic, m.qos)
		}
		if m.retained {
			t.Errorf("%s published retained, want not retained", m.topic)
		}
	}
}

func TestPublish_customBaseTopic_isHonoured(t *testing.T) {
	cfg := testConfig()
	cfg.BaseTopic = "sensors/radiation"

	s, client := newTestSink(t, cfg)
	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if _, ok := client.payloadFor("sensors/radiation/cpm"); !ok {
		t.Errorf("no publish to sensors/radiation/cpm, got %+v", client.sent())
	}
}

func TestPublish_nonFiniteValue_isSkippedNotPublished(t *testing.T) {
	s, client := newTestSink(t, testConfig())

	r := fullReading()
	r.MicroSievertsPerHour = math.NaN()
	r.AverageCPM = math.Inf(1)

	if err := s.Publish(context.Background(), r); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for _, topic := range []string{"gmc-exporter/usvh", "gmc-exporter/average_cpm"} {
		if payload, ok := client.payloadFor(topic); ok {
			t.Errorf("published %q to %s, want the non-finite value dropped", payload, topic)
		}
	}
}

// --- failure containment ---------------------------------------------------

func TestPublish_disconnected_returnsTransientError(t *testing.T) {
	s, client := newTestSink(t, testConfig())
	client.connected = false

	err := s.Publish(context.Background(), fullReading())
	if err == nil {
		t.Fatal("Publish returned nil while disconnected, want an error")
	}
	if len(client.sent()) != 0 {
		t.Errorf("published %d messages while disconnected, want 0", len(client.sent()))
	}
}

func TestPublish_brokerRejects_returnsErrorAndDoesNotPanic(t *testing.T) {
	s, client := newTestSink(t, testConfig())
	client.failWith = errors.New("broker refused the message")

	// A panic here would escape into the fan-out's recover and be reported as a
	// crashed sink, so the test asserts the error path is a plain error.
	err := s.Publish(context.Background(), fullReading())
	if err == nil {
		t.Fatal("Publish returned nil after a broker failure, want an error")
	}
	if !strings.Contains(err.Error(), "broker refused the message") {
		t.Errorf("error = %v, want it to wrap the broker failure", err)
	}
}

func TestPublish_firstFailureStopsRemainingTopics(t *testing.T) {
	s, client := newTestSink(t, testConfig())
	client.failWith = errors.New("nope")
	client.failTopic = "gmc-exporter/cpm"

	if err := s.Publish(context.Background(), fullReading()); err == nil {
		t.Fatal("Publish returned nil, want an error")
	}
	// One timeout per outage, not one per topic.
	if len(client.sent()) != 1 {
		t.Errorf("published %d messages after the first failure, want 1", len(client.sent()))
	}
}

func TestPublish_contextCancelled_returnsWithoutBlocking(t *testing.T) {
	s, client := newTestSink(t, testConfig())
	client.stall = true

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Publish(ctx, fullReading()) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Publish did not return after its context expired")
	}
}

func TestPublish_afterClose_returnsError(t *testing.T) {
	s, _ := newTestSink(t, testConfig())
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Publish(context.Background(), fullReading()); err == nil {
		t.Error("Publish returned nil after Close, want an error")
	}
}

// --- availability ----------------------------------------------------------

func TestOnConnected_publishesOnlineRetained(t *testing.T) {
	s, client := newTestSink(t, testConfig())
	s.onConnected()

	sent := client.sent()
	if len(sent) == 0 {
		t.Fatal("onConnected published nothing")
	}
	first := sent[0]
	if first.topic != "gmc-exporter/availability" {
		t.Errorf("first publish topic = %q, want the availability topic", first.topic)
	}
	if first.payload != "online" {
		t.Errorf("availability payload = %q, want %q", first.payload, "online")
	}
	if !first.retained {
		// Not retained means a Home Assistant that starts later never learns
		// the exporter is alive.
		t.Error("availability published without the retain flag")
	}
}

func TestClose_publishesOfflineRetainedAndDisconnects(t *testing.T) {
	s, client := newTestSink(t, testConfig())

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sent := client.sent()
	if len(sent) != 1 {
		t.Fatalf("Close published %d messages, want 1: %+v", len(sent), sent)
	}
	want := published{topic: "gmc-exporter/availability", qos: 1, retained: true, payload: "offline"}
	if sent[0] != want {
		t.Errorf("Close published %+v, want %+v", sent[0], want)
	}
	if client.disconnect != 1 {
		t.Errorf("Disconnect called %d times, want 1", client.disconnect)
	}
}

func TestClose_isIdempotent(t *testing.T) {
	s, client := newTestSink(t, testConfig())

	for i := 0; i < 3; i++ {
		if err := s.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	if len(client.sent()) != 1 {
		t.Errorf("published %d offline messages, want 1", len(client.sent()))
	}
}

func TestClose_neverConnected_succeeds(t *testing.T) {
	s, client := newTestSink(t, testConfig())
	client.connected = false

	if err := s.Close(); err != nil {
		t.Fatalf("Close on an unconnected sink: %v", err)
	}
	if len(client.sent()) != 0 {
		t.Errorf("published %d messages while disconnected, want 0", len(client.sent()))
	}
}

func TestClose_offlinePublishFails_stillDisconnects(t *testing.T) {
	s, client := newTestSink(t, testConfig())
	client.failWith = errors.New("broker gone")

	if err := s.Close(); err == nil {
		t.Error("Close returned nil after a failed offline publish, want an error")
	}
	if client.disconnect != 1 {
		t.Errorf("Disconnect called %d times, want 1 even after a publish failure", client.disconnect)
	}
}

// --- construction ----------------------------------------------------------

func TestNew_missingBroker_returnsError(t *testing.T) {
	cfg := testConfig()
	cfg.Broker = "   "

	if _, err := New(cfg, testDevice(), discardLogger()); err == nil {
		t.Fatal("New succeeded without a broker, want an error")
	}
}

func TestNew_unreachableBroker_startsAnyway(t *testing.T) {
	cfg := testConfig()
	// Port 1 on loopback refuses connections immediately, which stands in for
	// a broker that is not up yet when the container starts.
	cfg.Broker = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = time.Second

	logs := &syncBuffer{}
	s, err := New(cfg, testDevice(), slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatalf("New failed against an unreachable broker: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The failure surfaces as a transient publish error, not a startup failure.
	if err := s.Publish(context.Background(), fullReading()); err == nil {
		t.Error("Publish succeeded against an unreachable broker, want a transient error")
	}
}

func TestName_isStable(t *testing.T) {
	s, _ := newTestSink(t, testConfig())
	// The name becomes a metric label, so changing it breaks dashboards.
	if s.Name() != "mqtt" {
		t.Errorf("Name = %q, want %q", s.Name(), "mqtt")
	}
}

func TestBrokerURL_schemeFollowsTLS(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.MQTT
		want string
	}{
		{"plain", config.MQTT{Broker: "mqtt.lan", Port: 1883}, "tcp://mqtt.lan:1883"},
		{"tls", config.MQTT{Broker: "mqtt.lan", Port: 8883, TLS: true}, "ssl://mqtt.lan:8883"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := brokerURL(tt.cfg); got != tt.want {
				t.Errorf("brokerURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewSink_invalidQoS_returnsError(t *testing.T) {
	cfg := testConfig()
	cfg.QoS = 3

	if _, err := newSink(cfg, testDevice(), discardLogger()); err == nil {
		t.Fatal("newSink accepted qos 3, want an error")
	}
}

func TestSanitizeTopic_stripsWildcardsAndFallsBack(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"gmc-exporter", "gmc-exporter"},
		{"/sensors/radiation/", "sensors/radiation"},
		{"  spaced  ", "spaced"},
		{"bad/#/topic", "bad//topic"},
		{"+", "fallback"},
		{"", "fallback"},
	}
	for _, tt := range tests {
		if got := sanitizeTopic(tt.in, "fallback"); got != tt.want {
			t.Errorf("sanitizeTopic(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeID_keepsOnlyDiscoverySafeCharacters(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"gmc-exporter", "gmc-exporter"},
		{"gmc/exporter 1", "gmc_exporter_1"},
		{"...", defaultClientID},
		{"", defaultClientID},
	}
	for _, tt := range tests {
		if got := sanitizeID(tt.in); got != tt.want {
			t.Errorf("sanitizeID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- TLS -------------------------------------------------------------------

func TestTLSConfig_loadsCAAndClientCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeSelfSignedPair(t, dir)

	cfg := config.MQTT{
		Broker:     "mqtt.lan",
		TLS:        true,
		CACert:     certFile,
		ClientCert: config.ClientCert{CertFile: certFile, KeyFile: keyFile},
	}

	tlsCfg, err := tlsConfig(cfg)
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}
	if tlsCfg.RootCAs == nil {
		t.Error("RootCAs is nil, want the supplied CA certificate")
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("loaded %d client certificates, want 1", len(tlsCfg.Certificates))
	}
	if tlsCfg.ServerName != "mqtt.lan" {
		t.Errorf("ServerName = %q, want %q", tlsCfg.ServerName, "mqtt.lan")
	}
	if tlsCfg.MinVersion < 0x0303 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 or later", tlsCfg.MinVersion)
	}
}

func TestTLSConfig_missingCAFile_returnsError(t *testing.T) {
	cfg := config.MQTT{Broker: "mqtt.lan", TLS: true, CACert: filepath.Join(t.TempDir(), "absent.pem")}

	if _, err := tlsConfig(cfg); err == nil {
		t.Fatal("tlsConfig accepted a missing CA file, want an error rather than a silent fallback")
	}
}

func TestTLSConfig_nonPEMCAFile_returnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg := config.MQTT{Broker: "mqtt.lan", TLS: true, CACert: path}

	if _, err := tlsConfig(cfg); err == nil {
		t.Fatal("tlsConfig accepted a file with no certificates in it, want an error")
	}
}

func TestTLSConfig_halfClientPair_returnsError(t *testing.T) {
	cfg := config.MQTT{Broker: "mqtt.lan", TLS: true, ClientCert: config.ClientCert{CertFile: "cert.pem"}}

	if _, err := tlsConfig(cfg); err == nil {
		t.Fatal("tlsConfig accepted a certificate without its key, want an error")
	}
}

// writeSelfSignedPair produces a throwaway certificate and key so the TLS
// loading path is exercised for real rather than mocked.
func writeSelfSignedPair(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mqtt-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
	return certFile, keyFile
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- credential hygiene ----------------------------------------------------

// TestLogging_passwordNeverReachesLogs is the test that matters most here.
// Container logs get pasted into tickets and shipped to aggregators, so a
// password appearing once in a connect-failure line is a real disclosure.
func TestLogging_passwordNeverReachesLogs(t *testing.T) {
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := testConfig()
	cfg.Broker = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = time.Second

	// Connect path: a broker that refuses every attempt, so the connect-failure
	// and reconnect handlers all run.
	s, err := New(cfg, testDevice(), logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Publish(context.Background(), fullReading()); err == nil {
		t.Error("Publish succeeded against a refused connection, want an error")
	}
	_ = s.Close()

	// Failure path with a connected client, covering the publish-error and
	// discovery-error logging.
	failing, client := newTestSink(t, cfg)
	failing.log = logger.With("sink", Name)
	client.failWith = errors.New("broker refused the message")
	failing.onConnected()
	if err := failing.Publish(context.Background(), fullReading()); err == nil {
		t.Error("Publish succeeded against a failing client, want an error")
	}
	_ = failing.Close()

	// Give the paho reconnect goroutine a moment to log anything it still owes.
	time.Sleep(100 * time.Millisecond)

	out := logs.String()
	if out == "" {
		t.Fatal("captured no log output, so the assertion below would prove nothing")
	}
	if strings.Contains(out, canaryPassword) {
		t.Errorf("the MQTT password appears in log output:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1") {
		t.Errorf("expected the broker address in the logs, got:\n%s", out)
	}
}

func TestLogging_secretFormattingIsRedacted(t *testing.T) {
	// A Secret that lands in a log line through any of the usual routes must
	// print the placeholder, which is what lets the sink log its own config.
	secret := redact.New(canaryPassword)

	logs := &syncBuffer{}
	slog.New(slog.NewJSONHandler(logs, nil)).Info("config", "password", secret, "formatted", secret.String())

	if strings.Contains(logs.String(), canaryPassword) {
		t.Errorf("secret leaked through slog:\n%s", logs.String())
	}
	if secret.Reveal() != canaryPassword {
		t.Error("Reveal did not return the underlying password")
	}
}
