package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
)

// manufacturer is fixed: every device speaking GQ-RFC1201 is a GQ Electronics
// unit, and the model within that range comes from <GETVER>>.
const manufacturer = "GQ Electronics"

// Home Assistant units. µSv/h and CPM are written as Home Assistant expects to
// see them, including the micro sign rather than a Latin "u".
const (
	unitCPM          = "CPM"
	unitMicroSievert = "µSv/h"
	unitVolt         = "V"
	unitCelsius      = "°C"
)

// Home Assistant classes. Only the quantities with a standard device class get
// one; inventing a class for a count rate would make Home Assistant apply unit
// conversions that do not apply to it.
const (
	deviceClassVoltage     = "voltage"
	deviceClassTemperature = "temperature"
	stateClassMeasurement  = "measurement"
)

// haDevice is the device block. It is byte-for-byte identical in every sensor's
// config, which is what makes Home Assistant group the five entities under one
// device instead of creating five.
type haDevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	SWVersion    string   `json:"sw_version,omitempty"`
}

// haSensor is one MQTT sensor discovery payload.
//
// The availability fields are what stop a dead exporter from looking healthy:
// Home Assistant marks the entity unavailable the moment the broker delivers
// the retained will, instead of continuing to display the last dose rate.
type haSensor struct {
	Name                string   `json:"name"`
	UniqueID            string   `json:"unique_id"`
	StateTopic          string   `json:"state_topic"`
	AvailabilityTopic   string   `json:"availability_topic"`
	PayloadAvailable    string   `json:"payload_available"`
	PayloadNotAvailable string   `json:"payload_not_available"`
	DeviceClass         string   `json:"device_class,omitempty"`
	UnitOfMeasurement   string   `json:"unit_of_measurement"`
	StateClass          string   `json:"state_class"`
	Device              haDevice `json:"device"`
}

// sensorSpec describes one advertised sensor. Keeping the five in a table means
// the state topic a sensor is told to read is the same string Publish writes.
type sensorSpec struct {
	// key is the object id in the discovery topic and the unique_id suffix.
	key string
	// name is the entity name; Home Assistant prefixes it with the device name.
	name        string
	topic       func(t topics) string
	deviceClass string
	unit        string
}

// sensorSpecs is every sensor advertised, in a fixed order so the discovery
// topics are stable across restarts.
//
// CPM and µSv/h have no standard Home Assistant device class, so they carry
// none. They are still state_class "measurement", which is what puts them in
// the statistics engine and gives them long-term history.
var sensorSpecs = []sensorSpec{
	{
		key:   "cpm",
		name:  "CPM",
		topic: func(t topics) string { return t.cpm },
		unit:  unitCPM,
	},
	{
		key:   "usvh",
		name:  "Dose Rate",
		topic: func(t topics) string { return t.microSieverts },
		unit:  unitMicroSievert,
	},
	{
		key:   "average_cpm",
		name:  "Average CPM",
		topic: func(t topics) string { return t.averageCPM },
		unit:  unitCPM,
	},
	{
		key:         "voltage",
		name:        "Battery Voltage",
		topic:       func(t topics) string { return t.voltage },
		deviceClass: deviceClassVoltage,
		unit:        unitVolt,
	},
	{
		key:         "temperature",
		name:        "Temperature",
		topic:       func(t topics) string { return t.temperature },
		deviceClass: deviceClassTemperature,
		unit:        unitCelsius,
	},
}

// discoveryMessage is one rendered discovery config, ready to publish.
type discoveryMessage struct {
	topic   string
	payload []byte
}

// haDeviceBlock builds the shared device block.
func (s *Sink) haDeviceBlock() haDevice {
	return haDevice{
		Identifiers:  []string{s.deviceID},
		Name:         s.cfg.DeviceName,
		Manufacturer: manufacturer,
		Model:        s.device.Model,
		SWVersion:    s.device.Firmware,
	}
}

// discoveryTopic is where Home Assistant looks for one sensor's config.
func (s *Sink) discoveryTopic(key string) string {
	return fmt.Sprintf("%s/sensor/%s/%s/config", s.cfg.HAPrefix, s.deviceID, key)
}

// discoveryMessages renders every sensor's discovery config.
//
// It returns an error only if a payload will not marshal, which would mean a
// programming error in the structs above rather than anything a broker or the
// device did.
func (s *Sink) discoveryMessages() ([]discoveryMessage, error) {
	device := s.haDeviceBlock()
	out := make([]discoveryMessage, 0, len(sensorSpecs))

	for _, spec := range sensorSpecs {
		sensor := haSensor{
			Name:                spec.name,
			UniqueID:            s.deviceID + "_" + spec.key,
			StateTopic:          spec.topic(s.topics),
			AvailabilityTopic:   s.topics.availability,
			PayloadAvailable:    payloadOnline,
			PayloadNotAvailable: payloadOffline,
			DeviceClass:         spec.deviceClass,
			UnitOfMeasurement:   spec.unit,
			// Every quantity here is an instantaneous reading, never a
			// cumulative total, so they all measure rather than accumulate.
			StateClass: stateClassMeasurement,
			Device:     device,
		}

		payload, err := json.Marshal(sensor)
		if err != nil {
			return nil, fmt.Errorf("mqtt: marshal discovery for %s: %w", spec.key, err)
		}
		out = append(out, discoveryMessage{topic: s.discoveryTopic(spec.key), payload: payload})
	}
	return out, nil
}

// publishDiscovery advertises all five sensors.
//
// The configs are retained so Home Assistant picks the sensors up whenever it
// next starts, not only if it happened to be listening at this moment.
func (s *Sink) publishDiscovery(ctx context.Context) error {
	messages, err := s.discoveryMessages()
	if err != nil {
		return err
	}
	for _, m := range messages {
		if err := s.publishRetained(ctx, m.topic, string(m.payload)); err != nil {
			return err
		}
	}
	return nil
}
