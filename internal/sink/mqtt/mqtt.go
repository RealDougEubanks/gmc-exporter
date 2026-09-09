// Package mqtt publishes readings to an MQTT broker, optionally advertising the
// sensors to Home Assistant.
//
// The broker and Home Assistant both live elsewhere on the network, so every
// address is configuration and nothing is assumed to be reachable at startup. A
// broker that is down when the container starts must not prevent the exporter
// from running: the connection is established in the background and Publish
// reports a transient error until it succeeds.
package mqtt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink"
)

// Sink implements sink.Sink. Asserting it here means a change to the interface
// breaks the build rather than the wiring in main.
var _ sink.Sink = (*Sink)(nil)

// Name is the sink's stable identifier, used as a metric label.
const Name = "mqtt"

// Availability payloads. These are the values Home Assistant expects by
// default, and the discovery messages state them explicitly so the contract
// does not rest on a default that could change.
const (
	payloadOnline  = "online"
	payloadOffline = "offline"
)

// defaultBaseTopic and defaultHAPrefix mirror the config defaults so a Sink
// built from a zero-valued config still produces well-formed topics.
const (
	defaultBaseTopic  = "gmc-exporter"
	defaultHAPrefix   = "homeassistant"
	defaultClientID   = "gmc-exporter"
	defaultDeviceName = "Geiger Counter"
	defaultModel      = "GMC-320"
)

// Reconnect bounds. The initial retry is short so a broker that is merely
// restarting is picked up quickly, and the cap keeps a long outage from
// generating a connection attempt every second for days.
const (
	connectRetryInterval = 5 * time.Second
	maxReconnectInterval = 2 * time.Minute
)

// publisher is the slice of paho.Client this sink actually uses.
//
// Narrowing the dependency to three methods is what makes the sink testable
// without a broker: the tests inject a fake that records every publish instead
// of opening a socket.
//
// The readiness check is IsConnectionOpen rather than IsConnected because paho
// reports IsConnected true while it is still retrying a connection that has
// never succeeded, which would let Publish queue messages against a broker that
// was never reached.
type publisher interface {
	Publish(topic string, qos byte, retained bool, payload any) paho.Token
	IsConnectionOpen() bool
	Disconnect(quiesce uint)
}

// Device describes the hardware being reported on. It populates the Home
// Assistant device block so every sensor groups under one device.
type Device struct {
	// Model is the hardware model reported by <GETVER>>, e.g. "GMC-320".
	Model string
	// Firmware is the firmware revision, e.g. "Re 3.03".
	Firmware string
	// Serial identifies this particular unit. When present it becomes the
	// device identifier, so re-deploying the exporter under a new client ID
	// still updates the same Home Assistant device.
	Serial string
}

// Sink publishes readings to an MQTT broker.
type Sink struct {
	cfg    config.MQTT
	device Device
	log    *slog.Logger

	client publisher
	qos    byte
	topics topics

	// deviceID is the sanitised identifier shared by the discovery topics and
	// every unique_id.
	deviceID string

	// brokerURL is safe to log: paho carries credentials in its options, not
	// in the URL.
	brokerURL string

	mu     sync.Mutex
	closed bool
}

// topics holds every topic this sink writes, resolved once at construction so a
// publish does no string building beyond the payload.
type topics struct {
	availability  string
	cpm           string
	averageCPM    string
	microSieverts string
	voltage       string
	temperature   string
}

// New creates a Sink and starts connecting in the background.
//
// It returns an error only for configuration that can never work, such as a
// missing broker or an unreadable certificate. An unreachable broker is not
// such a case: connecting is retried indefinitely and Publish reports the
// interim failures, because refusing to start would turn a broker restart into
// an exporter outage.
func New(cfg config.MQTT, device Device, log *slog.Logger) (*Sink, error) {
	if log == nil {
		log = slog.Default()
	}
	if strings.TrimSpace(cfg.Broker) == "" {
		return nil, errors.New("mqtt: broker is required")
	}

	s, err := newSink(cfg, device, log)
	if err != nil {
		return nil, err
	}

	opts, err := s.clientOptions()
	if err != nil {
		return nil, err
	}

	client := paho.NewClient(opts)
	s.client = client

	// The token is deliberately not waited on. With SetConnectRetry the client
	// keeps trying in the background, so waiting here would only delay startup
	// while producing the same eventual outcome.
	client.Connect()

	s.log.Info("mqtt sink started",
		"broker", s.brokerURL,
		"base_topic", s.cfg.BaseTopic,
		"ha_discovery", s.cfg.HADiscovery)
	return s, nil
}

// newSink builds the sink without a client, so both New and the tests share one
// definition of how configuration maps to topics.
func newSink(cfg config.MQTT, device Device, log *slog.Logger) (*Sink, error) {
	cfg = withDefaults(cfg)
	device = device.withDefaults()

	if cfg.QoS < 0 || cfg.QoS > 2 {
		return nil, fmt.Errorf("mqtt: qos must be 0, 1 or 2, got %d", cfg.QoS)
	}

	base := cfg.BaseTopic
	s := &Sink{
		cfg:      cfg,
		device:   device,
		log:      log.With("sink", Name),
		qos:      byte(cfg.QoS),
		deviceID: sanitizeID(firstNonEmpty(device.Serial, cfg.ClientID, defaultClientID)),
		topics: topics{
			availability:  base + "/availability",
			cpm:           base + "/cpm",
			averageCPM:    base + "/average_cpm",
			microSieverts: base + "/usvh",
			voltage:       base + "/voltage",
			temperature:   base + "/temperature",
		},
		brokerURL: brokerURL(cfg),
	}
	return s, nil
}

// withDefaults fills the fields a zero-valued config leaves empty. Config.Load
// supplies all of these, but a Sink constructed directly must still produce
// valid topics rather than ones starting with a slash.
func withDefaults(cfg config.MQTT) config.MQTT {
	cfg.BaseTopic = sanitizeTopic(cfg.BaseTopic, defaultBaseTopic)
	cfg.HAPrefix = sanitizeTopic(cfg.HAPrefix, defaultHAPrefix)
	cfg.ClientID = firstNonEmpty(strings.TrimSpace(cfg.ClientID), defaultClientID)
	cfg.DeviceName = firstNonEmpty(strings.TrimSpace(cfg.DeviceName), defaultDeviceName)
	if cfg.Port <= 0 {
		if cfg.TLS {
			cfg.Port = 8883
		} else {
			cfg.Port = 1883
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	return cfg
}

func (d Device) withDefaults() Device {
	d.Model = firstNonEmpty(strings.TrimSpace(d.Model), defaultModel)
	d.Firmware = strings.TrimSpace(d.Firmware)
	d.Serial = strings.TrimSpace(d.Serial)
	return d
}

// brokerURL renders the address paho dials. The scheme is the only thing TLS
// changes at this layer; the certificate handling lives in tlsConfig.
func brokerURL(cfg config.MQTT) string {
	scheme := "tcp"
	if cfg.TLS {
		scheme = "ssl"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, cfg.Broker, cfg.Port)
}

// clientOptions translates config into paho's options, including the last will
// that makes Home Assistant mark the sensors unavailable when this process
// dies.
func (s *Sink) clientOptions() (*paho.ClientOptions, error) {
	opts := paho.NewClientOptions().
		AddBroker(s.brokerURL).
		SetClientID(s.cfg.ClientID).
		SetConnectTimeout(s.cfg.Timeout).
		SetWriteTimeout(s.cfg.Timeout).
		// A clean session avoids the broker queueing months of stale readings
		// for a client that was offline; retained topics already carry the
		// current value.
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(connectRetryInterval).
		SetMaxReconnectInterval(maxReconnectInterval).
		SetOrderMatters(false)

	// The will is retained so a subscriber that connects after this process
	// died still sees "offline". Without it Home Assistant would display the
	// last reading forever, which is worse than showing nothing: a stale dose
	// rate looks like a live one.
	opts.SetWill(s.topics.availability, payloadOffline, s.qos, true)

	if s.cfg.Username != "" {
		opts.SetUsername(s.cfg.Username)
	}
	if !s.cfg.Password.IsZero() {
		// The only place the password is revealed, and it goes to the client
		// rather than to a log.
		opts.SetPassword(s.cfg.Password.Reveal())
	}

	if s.cfg.TLS {
		tlsCfg, err := tlsConfig(s.cfg)
		if err != nil {
			return nil, err
		}
		opts.SetTLSConfig(tlsCfg)
	}

	opts.SetOnConnectHandler(func(paho.Client) { s.onConnected() })
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		// Logged and dropped: paho reconnects on its own, and the poll loop
		// must not learn about broker trouble.
		s.log.Warn("mqtt connection lost", "broker", s.brokerURL, "error", err)
	})
	opts.SetReconnectingHandler(func(paho.Client, *paho.ClientOptions) {
		s.log.Info("mqtt reconnecting", "broker", s.brokerURL)
	})
	return opts, nil
}

// tlsConfig builds the TLS settings from the optional CA certificate and the
// optional client certificate pair.
//
// An empty CACert means the system trust store, which is correct for a broker
// with a publicly issued certificate. A private CA has to be supplied, and a
// CA file that cannot be read is a hard failure: silently falling back to the
// system pool would connect to a broker nobody vouched for.
func tlsConfig(cfg config.MQTT) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: cfg.Broker,
	}

	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("mqtt: read CA certificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mqtt: no certificates found in %s", cfg.CACert)
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.ClientCert.CertFile != "" || cfg.ClientCert.KeyFile != "" {
		if cfg.ClientCert.CertFile == "" || cfg.ClientCert.KeyFile == "" {
			return nil, errors.New("mqtt: client certificate and key must be set together")
		}
		pair, err := tls.LoadX509KeyPair(cfg.ClientCert.CertFile, cfg.ClientCert.KeyFile)
		if err != nil {
			// The error names the files, never their contents, so a malformed
			// private key cannot reach a log.
			return nil, fmt.Errorf("mqtt: load client certificate %s: %w", cfg.ClientCert.CertFile, err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}

	return tlsCfg, nil
}

// Name identifies the sink.
func (s *Sink) Name() string { return Name }

// Publish sends one reading, one topic per value.
//
// Optional values the device did not supply are omitted rather than published
// as zero: a retained "0.0" on the temperature topic is indistinguishable from
// a real freezing reading.
func (s *Sink) Publish(ctx context.Context, r reading.Reading) error {
	if s.isClosed() {
		return errors.New("mqtt: sink is closed")
	}
	if !s.client.IsConnectionOpen() {
		// Transient by construction: paho is still retrying. The fan-out logs
		// this and moves on to the next poll.
		return fmt.Errorf("mqtt: not connected to %s", s.brokerURL)
	}

	values := []message{
		{topic: s.topics.cpm, payload: strconv.FormatUint(uint64(r.CPM), 10), finite: true},
		newMessage(s.topics.averageCPM, r.AverageCPM, 2),
		newMessage(s.topics.microSieverts, r.MicroSievertsPerHour, 4),
	}
	if r.Voltage.Valid {
		values = append(values, newMessage(s.topics.voltage, r.Voltage.Value, 2))
	}
	if r.TemperatureC.Valid {
		values = append(values, newMessage(s.topics.temperature, r.TemperatureC.Value, 1))
	}

	for _, v := range values {
		if !v.finite {
			// A NaN or infinity means the calibration produced nonsense. Drop
			// the value rather than retain "NaN" on a topic Home Assistant
			// will try to parse as a number.
			s.log.Warn("skipping non-finite value", "topic", v.topic)
			continue
		}
		if err := s.publish(ctx, v.topic, v.payload, s.cfg.Retain); err != nil {
			// Returning on the first failure keeps a broker outage bounded by
			// one timeout instead of one per topic.
			return err
		}
	}
	return nil
}

// Close announces the shutdown and disconnects.
//
// The retained "offline" is the whole point: it is what tells Home Assistant to
// stop showing the last reading as if it were current.
func (s *Sink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.client == nil {
		return nil
	}

	var err error
	if s.client.IsConnectionOpen() {
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
		err = s.publishRetained(ctx, s.topics.availability, payloadOffline)
		cancel()
		if err != nil {
			err = fmt.Errorf("mqtt: publish offline: %w", err)
		}
	}

	// Disconnect regardless, so a broker that would not accept the farewell
	// still gets the socket closed.
	s.client.Disconnect(uint(s.cfg.Timeout / time.Millisecond))
	return err
}

// onConnected runs on every successful connect, including reconnects.
//
// Both the availability message and the discovery configs are republished each
// time, because a broker that restarted lost every retained message it held.
func (s *Sink) onConnected() {
	if s.isClosed() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()

	if err := s.publishRetained(ctx, s.topics.availability, payloadOnline); err != nil {
		s.log.Error("publish availability failed", "topic", s.topics.availability, "error", err)
		return
	}
	s.log.Info("mqtt connected", "broker", s.brokerURL)

	if !s.cfg.HADiscovery {
		return
	}
	if err := s.publishDiscovery(ctx); err != nil {
		// Discovery failing costs auto-configuration, not data: the state
		// topics still carry readings for anyone already subscribed.
		s.log.Error("publish home assistant discovery failed", "error", err)
		return
	}
	s.log.Info("published home assistant discovery",
		"prefix", s.cfg.HAPrefix, "device_id", s.deviceID, "sensors", len(sensorSpecs))
}

// publishRetained sends a retained message, used for availability and
// discovery, whose value must survive a subscriber connecting later.
func (s *Sink) publishRetained(ctx context.Context, topic, payload string) error {
	return s.publish(ctx, topic, payload, true)
}

// publish sends one message and waits for the broker.
//
// The wait is bounded by the caller's context and, independently, by the
// configured timeout. Paho accepts a publish while it is still reconnecting and
// completes the token only once the message reaches the broker, so a publish
// with an unbounded context would otherwise block for the whole length of an
// outage.
func (s *Sink) publish(ctx context.Context, topic, payload string, retained bool) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	token := s.client.Publish(topic, s.qos, retained, payload)
	if token == nil {
		return fmt.Errorf("mqtt: publish %s: client returned no token", topic)
	}

	select {
	case <-token.Done():
		if err := token.Error(); err != nil {
			return fmt.Errorf("mqtt: publish %s: %w", topic, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mqtt: publish %s: %w", topic, ctx.Err())
	}
}

func (s *Sink) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// message is one topic and its rendered payload. finite records whether the
// source value was a real number, so the caller can drop it without having to
// re-inspect the reading.
type message struct {
	topic   string
	payload string
	finite  bool
}

// newMessage renders a float for a numeric topic.
//
// The precision is per-quantity rather than full float precision: Home
// Assistant graphs "0.0512" far better than "0.05119999999999999", and the
// extra digits carry no information the device actually measured.
func newMessage(topic string, v float64, precision int) message {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return message{topic: topic}
	}
	return message{
		topic:   topic,
		payload: strconv.FormatFloat(v, 'f', precision, 64),
		finite:  true,
	}
}

// sanitizeTopic removes what MQTT forbids in a publish topic and falls back to
// a default when nothing usable is left.
//
// Wildcards are legal in a subscription and illegal in a publish, so a base
// topic containing one would make every publish fail at the broker with a
// message that is hard to trace back to configuration.
func sanitizeTopic(topic, fallback string) string {
	topic = strings.TrimSpace(topic)
	topic = strings.NewReplacer("#", "", "+", "", "\x00", "").Replace(topic)
	topic = strings.Trim(topic, "/")
	if topic == "" {
		return fallback
	}
	return topic
}

// sanitizeID reduces an identifier to the characters Home Assistant accepts in
// a discovery topic's node id and in a unique_id.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return defaultClientID
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
