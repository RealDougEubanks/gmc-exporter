package mqtt

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// decodeDiscovery collects every retained discovery config the sink published,
// keyed by topic and decoded as generic JSON so the assertions are about what
// Home Assistant actually receives rather than about this package's structs.
func decodeDiscovery(t *testing.T, client *fakeClient, prefix string) map[string]map[string]any {
	t.Helper()

	out := make(map[string]map[string]any)
	for _, m := range client.sent() {
		if !strings.HasPrefix(m.topic, prefix+"/") {
			continue
		}
		if !m.retained {
			// A discovery config that is not retained is lost the moment Home
			// Assistant restarts.
			t.Errorf("discovery config on %s was published without the retain flag", m.topic)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(m.payload), &payload); err != nil {
			t.Fatalf("discovery payload on %s is not valid JSON: %v\n%s", m.topic, err, m.payload)
		}
		out[m.topic] = payload
	}
	return out
}

func publishDiscoveryForTest(t *testing.T) (*Sink, *fakeClient, map[string]map[string]any) {
	t.Helper()

	s, client := newTestSink(t, testConfig())
	if err := s.publishDiscovery(context.Background()); err != nil {
		t.Fatalf("publishDiscovery: %v", err)
	}
	return s, client, decodeDiscovery(t, client, "homeassistant")
}

func TestDiscovery_publishesOneConfigPerSensor(t *testing.T) {
	_, _, configs := publishDiscoveryForTest(t)

	want := []string{
		"homeassistant/sensor/a1b2c3/cpm/config",
		"homeassistant/sensor/a1b2c3/usvh/config",
		"homeassistant/sensor/a1b2c3/average_cpm/config",
		"homeassistant/sensor/a1b2c3/voltage/config",
		"homeassistant/sensor/a1b2c3/temperature/config",
	}
	if len(configs) != len(want) {
		t.Fatalf("published %d discovery configs, want %d", len(configs), len(want))
	}
	for _, topic := range want {
		if _, ok := configs[topic]; !ok {
			t.Errorf("no discovery config on %s", topic)
		}
	}
}

func TestDiscovery_temperaturePayloadMatchesHAConventions(t *testing.T) {
	_, _, configs := publishDiscoveryForTest(t)

	got := configs["homeassistant/sensor/a1b2c3/temperature/config"]
	want := map[string]any{
		"name":                  "Temperature",
		"unique_id":             "a1b2c3_temperature",
		"state_topic":           "gmc-exporter/temperature",
		"availability_topic":    "gmc-exporter/availability",
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device_class":          "temperature",
		"unit_of_measurement":   "°C",
		"state_class":           "measurement",
		"device": map[string]any{
			"identifiers":  []any{"a1b2c3"},
			"name":         "Geiger Counter",
			"manufacturer": "GQ Electronics",
			"model":        "GMC-320",
			"sw_version":   "Re 3.03",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("temperature discovery payload:\ngot  %#v\nwant %#v", got, want)
	}
}

func TestDiscovery_countRatesCarryNoDeviceClass(t *testing.T) {
	_, _, configs := publishDiscoveryForTest(t)

	tests := []struct {
		topic string
		unit  string
	}{
		{"homeassistant/sensor/a1b2c3/cpm/config", "CPM"},
		{"homeassistant/sensor/a1b2c3/average_cpm/config", "CPM"},
		{"homeassistant/sensor/a1b2c3/usvh/config", "µSv/h"},
	}
	for _, tt := range tests {
		payload := configs[tt.topic]
		// Home Assistant has no device class for a count rate, and inventing
		// one makes it apply conversions that do not apply.
		if _, present := payload["device_class"]; present {
			t.Errorf("%s carries device_class %v, want it omitted", tt.topic, payload["device_class"])
		}
		if payload["unit_of_measurement"] != tt.unit {
			t.Errorf("%s unit_of_measurement = %v, want %q", tt.topic, payload["unit_of_measurement"], tt.unit)
		}
		if payload["state_class"] != "measurement" {
			t.Errorf("%s state_class = %v, want %q", tt.topic, payload["state_class"], "measurement")
		}
	}
}

func TestDiscovery_voltageUsesVoltageDeviceClass(t *testing.T) {
	_, _, configs := publishDiscoveryForTest(t)

	payload := configs["homeassistant/sensor/a1b2c3/voltage/config"]
	if payload["device_class"] != "voltage" {
		t.Errorf("device_class = %v, want %q", payload["device_class"], "voltage")
	}
	if payload["unit_of_measurement"] != "V" {
		t.Errorf("unit_of_measurement = %v, want %q", payload["unit_of_measurement"], "V")
	}
}

func TestDiscovery_deviceBlockIsIdenticalAcrossSensors(t *testing.T) {
	_, _, configs := publishDiscoveryForTest(t)

	var reference map[string]any
	var referenceTopic string
	for topic, payload := range configs {
		device, ok := payload["device"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no device block", topic)
		}
		if reference == nil {
			reference, referenceTopic = device, topic
			continue
		}
		// Any difference here and Home Assistant creates a separate device per
		// sensor instead of grouping the five.
		if !reflect.DeepEqual(device, reference) {
			t.Errorf("device block differs between %s and %s:\n%#v\n%#v",
				referenceTopic, topic, reference, device)
		}
	}
	if reference == nil {
		t.Fatal("no discovery configs were published")
	}
}

func TestDiscovery_everySensorHasTheAvailabilityTopic(t *testing.T) {
	_, _, configs := publishDiscoveryForTest(t)

	for topic, payload := range configs {
		if payload["availability_topic"] != "gmc-exporter/availability" {
			// Without this, a dead exporter leaves its last dose rate on
			// display looking like a live one.
			t.Errorf("%s availability_topic = %v, want %q",
				topic, payload["availability_topic"], "gmc-exporter/availability")
		}
	}
}

func TestDiscovery_uniqueIDsAreDistinct(t *testing.T) {
	_, _, configs := publishDiscoveryForTest(t)

	seen := make(map[string]string, len(configs))
	for topic, payload := range configs {
		id, ok := payload["unique_id"].(string)
		if !ok || id == "" {
			t.Errorf("%s has no unique_id", topic)
			continue
		}
		if other, dup := seen[id]; dup {
			t.Errorf("unique_id %q is shared by %s and %s", id, other, topic)
		}
		seen[id] = topic
	}
	if len(seen) != len(sensorSpecs) {
		t.Errorf("collected %d unique_ids, want %d", len(seen), len(sensorSpecs))
	}
}

func TestDiscovery_stateTopicsMatchWhatPublishWrites(t *testing.T) {
	s, client, configs := publishDiscoveryForTest(t)

	// The discovery configs are only useful if they name topics the sink
	// actually writes to, so this compares the two directly.
	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	written := make(map[string]bool)
	for _, m := range client.sent() {
		written[m.topic] = true
	}

	for topic, payload := range configs {
		state, _ := payload["state_topic"].(string)
		if !written[state] {
			t.Errorf("%s advertises state_topic %q, which Publish never writes", topic, state)
		}
	}
}

func TestDiscovery_customPrefixAndDeviceName(t *testing.T) {
	cfg := testConfig()
	cfg.HAPrefix = "ha"
	cfg.DeviceName = "Basement Geiger"

	s, client := newTestSink(t, cfg)
	if err := s.publishDiscovery(context.Background()); err != nil {
		t.Fatalf("publishDiscovery: %v", err)
	}

	configs := decodeDiscovery(t, client, "ha")
	if len(configs) != len(sensorSpecs) {
		t.Fatalf("published %d configs under the custom prefix, want %d", len(configs), len(sensorSpecs))
	}
	for topic, payload := range configs {
		device := payload["device"].(map[string]any)
		if device["name"] != "Basement Geiger" {
			t.Errorf("%s device name = %v, want %q", topic, device["name"], "Basement Geiger")
		}
	}
}

func TestOnConnected_discoveryDisabled_publishesOnlyAvailability(t *testing.T) {
	cfg := testConfig()
	cfg.HADiscovery = false

	s, client := newTestSink(t, cfg)
	s.onConnected()

	sent := client.sent()
	if len(sent) != 1 {
		t.Fatalf("published %d messages with discovery disabled, want 1: %+v", len(sent), sent)
	}
	if sent[0].topic != "gmc-exporter/availability" {
		t.Errorf("published %q, want only the availability topic", sent[0].topic)
	}
}

func TestOnConnected_republishesDiscoveryOnEveryConnect(t *testing.T) {
	s, client := newTestSink(t, testConfig())

	// A broker restart drops every retained message it held, so a reconnect has
	// to advertise the sensors again.
	s.onConnected()
	first := len(client.sent())
	s.onConnected()

	if got := len(client.sent()); got != 2*first {
		t.Errorf("published %d messages over two connects, want %d", got, 2*first)
	}
}

func TestDiscovery_deviceIDFallsBackToClientIDWithoutSerial(t *testing.T) {
	logger := discardLogger()
	cfg := testConfig()
	cfg.ClientID = "attic probe"

	s, err := newSink(cfg, Device{Model: "GMC-320"}, logger)
	if err != nil {
		t.Fatalf("newSink: %v", err)
	}
	client := &fakeClient{connected: true}
	s.client = client

	if err := s.publishDiscovery(context.Background()); err != nil {
		t.Fatalf("publishDiscovery: %v", err)
	}

	configs := decodeDiscovery(t, client, "homeassistant")
	topic := "homeassistant/sensor/attic_probe/cpm/config"
	payload, ok := configs[topic]
	if !ok {
		t.Fatalf("no config on %s, got %v", topic, configs)
	}
	device := payload["device"].(map[string]any)
	if _, present := device["sw_version"]; present {
		t.Errorf("sw_version present without a known firmware: %v", device["sw_version"])
	}
}
