// Package poller drives the sampling loop: it owns the connection to the
// device, reads one sample per tick, and hands it to the sinks.
//
// Its guiding constraint is that this process is expected to run unattended for
// months. Nothing recoverable is allowed to stop it. A failed read, a
// disconnected adapter, an unreachable backend and an outright panic all
// degrade to a logged, skipped reading rather than an exit.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/gmc"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
)

// Recorder receives poll outcomes so they can be surfaced as metrics, without
// the poller depending on any particular metrics implementation.
type Recorder interface {
	RecordPollSuccess(t time.Time)
	RecordPollFailure()
}

// Publisher is the fan-out the poller sends readings to. It matches
// sink.Set.Publish, and is an interface here so the poller can be tested
// without constructing real sinks.
type Publisher interface {
	Publish(ctx context.Context, r reading.Reading)
}

// Opener creates a port. It is injectable so tests can supply a fake device
// without a serial adapter present.
type Opener func(cfg serialport.Config) (serialport.Port, error)

// Poller samples the device on a fixed interval.
type Poller struct {
	cfg   *config.Config
	log   *slog.Logger
	open  Opener
	sinks Publisher
	rec   Recorder

	average *reading.Average

	mu          sync.RWMutex
	port        serialport.Port
	device      *gmc.Device
	calibration gmc.Calibration
	info        DeviceInfo
	lastSuccess time.Time
	lastError   string
}

// DeviceInfo is the identity read once per connection.
type DeviceInfo struct {
	Model    string
	Firmware string
	Serial   string
	// Version is the full version string exactly as the device reported it,
	// kept because existing dashboards commonly label series with it.
	Version string
}

// New builds a poller. The opener defaults to the real serial port.
func New(cfg *config.Config, log *slog.Logger, sinks Publisher, rec Recorder, open Opener) *Poller {
	if open == nil {
		open = serialport.Open
	}
	if log == nil {
		log = slog.Default()
	}
	return &Poller{
		cfg:     cfg,
		log:     log,
		open:    open,
		sinks:   sinks,
		rec:     rec,
		average: reading.NewAverage(cfg.Poll.AverageWindow),
	}
}

// Run samples until ctx is cancelled, then closes the port.
//
// Run returns nil on a clean shutdown. It returns an error only for a condition
// that genuinely cannot be recovered from by retrying, which in practice means
// never once the loop is running: a device that cannot be opened is retried
// with backoff rather than treated as fatal.
func (p *Poller) Run(ctx context.Context) error {
	defer p.closePort()

	// Connect before the first tick so startup problems surface immediately
	// rather than a poll interval later. A failure here is not fatal; the
	// loop retries.
	if err := p.connect(ctx); err != nil {
		p.log.Error("initial connection failed, will keep retrying",
			"port", p.cfg.Serial.Port, "error", err)
	}

	ticker := time.NewTicker(p.cfg.Poll.Interval)
	defer ticker.Stop()

	// Sample once immediately so a container restart produces data straight
	// away instead of after a full interval of silence.
	p.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poll loop stopping", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

// tick performs one poll cycle, containing any panic within it.
//
// The recover is deliberate belt-and-braces. Every parser validates length
// before indexing, so a panic should be impossible; but "should be impossible"
// is exactly the class of assumption that takes down long-running processes at
// three in the morning. Turning an unanticipated panic into a skipped reading
// costs nothing and removes a whole category of outage.
func (p *Poller) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("poll cycle panicked, reading skipped", "panic", r)
			p.recordFailure(fmt.Sprintf("panic: %v", r))
		}
	}()

	sample, err := p.sample(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		p.log.Warn("reading skipped", "error", err)
		p.recordFailure(err.Error())

		// A transport-level failure may mean the adapter went away. Drop the
		// connection so the next cycle reopens it.
		if isConnectionFailure(err) {
			p.log.Warn("dropping connection so it can be re-established",
				"port", p.cfg.Serial.Port)
			p.closePort()
		}
		return
	}

	p.recordSuccess(sample.Timestamp)
	p.log.Info("reading",
		"cpm", sample.CPM,
		"usv_per_hour", sample.MicroSievertsPerHour,
		"average_cpm", sample.AverageCPM,
		"volts", sample.Voltage.Or(0),
		"temperature_c", sample.TemperatureC.Or(0))

	if p.sinks != nil {
		p.sinks.Publish(ctx, sample)
	}
}

// sample reads one complete measurement, connecting first if needed.
func (p *Poller) sample(ctx context.Context) (reading.Reading, error) {
	if err := p.connect(ctx); err != nil {
		return reading.Reading{}, err
	}

	p.mu.RLock()
	device, calibration, info := p.device, p.calibration, p.info
	p.mu.RUnlock()

	if device == nil {
		return reading.Reading{}, errors.New("poller: device is not connected")
	}

	// CPM is the only mandatory value. If it fails there is no reading worth
	// publishing, so this is the one error that aborts the cycle.
	raw, err := device.Exchange(ctx, gmc.CmdGetCPM)
	if err != nil {
		return reading.Reading{}, fmt.Errorf("reading CPM: %w", err)
	}
	cpm, err := gmc.ParseCPM(raw)
	if err != nil {
		return reading.Reading{}, err
	}

	sample := reading.Reading{
		Timestamp:            time.Now().UTC(),
		CPM:                  cpm,
		AverageCPM:           p.average.Add(float64(cpm)),
		MicroSievertsPerHour: calibration.MicroSievertsPerHour(float64(cpm)),
		Voltage:              reading.None[float64](),
		TemperatureC:         reading.None[float64](),
		Device: reading.DeviceIdentity{
			Model:    info.Model,
			Firmware: info.Firmware,
			Serial:   info.Serial,
			Version:  info.Version,
		},
	}

	// Voltage and temperature are supplementary. A failure to read either is
	// logged and the field is left absent, because losing a temperature is no
	// reason to discard a perfectly good count rate.
	if v, err := p.readVoltage(ctx, device); err != nil {
		p.log.Debug("voltage unavailable this cycle", "error", err)
	} else {
		sample.Voltage = reading.Some(v)
	}

	if t, err := p.readTemperature(ctx, device); err != nil {
		p.log.Debug("temperature unavailable this cycle", "error", err)
	} else {
		sample.TemperatureC = reading.Some(t)
	}

	return sample, nil
}

func (p *Poller) readVoltage(ctx context.Context, d *gmc.Device) (float64, error) {
	raw, err := d.Exchange(ctx, gmc.CmdGetVoltage)
	if err != nil {
		return 0, err
	}
	return gmc.ParseVoltage(raw)
}

func (p *Poller) readTemperature(ctx context.Context, d *gmc.Device) (float64, error) {
	raw, err := d.Exchange(ctx, gmc.CmdGetTemperature)
	if err != nil {
		return 0, err
	}
	return gmc.ParseTemperature(raw)
}

// connect opens the port and loads device identity and calibration, if not
// already connected.
func (p *Poller) connect(ctx context.Context) error {
	p.mu.RLock()
	connected := p.device != nil
	p.mu.RUnlock()
	if connected {
		return nil
	}

	port, err := p.open(serialport.Config{
		Path:        p.cfg.Serial.Port,
		Baud:        p.cfg.Serial.Baud,
		ReadTimeout: p.cfg.Serial.ReadTimeout,
	})
	if err != nil {
		return fmt.Errorf("opening %s: %w", p.cfg.Serial.Port, err)
	}

	device := gmc.NewDevice(port, p.cfg.Serial.ResponseTimeout)

	info := p.readIdentity(ctx, device)
	calibration, err := p.loadCalibration(ctx, device)
	if err != nil {
		_ = closePort(port)
		return err
	}

	p.mu.Lock()
	p.port, p.device, p.calibration, p.info = port, device, calibration, info
	p.mu.Unlock()

	p.log.Info("connected to device",
		"port", p.cfg.Serial.Port,
		"model", info.Model,
		"firmware", info.Firmware,
		"serial", info.Serial,
		"calibration", calibration.String())
	return nil
}

// readIdentity reads model, firmware and serial. None of these are essential,
// so failures are logged and left blank rather than preventing a connection.
func (p *Poller) readIdentity(ctx context.Context, d *gmc.Device) DeviceInfo {
	var info DeviceInfo

	if raw, err := d.Exchange(ctx, gmc.CmdGetVersion); err != nil {
		p.log.Warn("could not read device version", "error", err)
	} else if v, err := gmc.ParseVersion(raw); err != nil {
		p.log.Warn("could not parse device version", "error", err)
	} else {
		info.Model, info.Firmware, info.Version = v.Model, v.Firmware, v.Raw
	}

	if raw, err := d.Exchange(ctx, gmc.CmdGetSerial); err != nil {
		p.log.Warn("could not read device serial", "error", err)
	} else if s, err := gmc.ParseSerial(raw); err != nil {
		p.log.Warn("could not parse device serial", "error", err)
	} else {
		info.Serial = s
	}

	return info
}

// loadCalibration resolves the CPM-to-dose table.
//
// A configured override wins, and a bad override is fatal to the connection
// rather than silently ignored: an operator who set it meant it, and quietly
// falling back would publish doses computed from a different table than they
// think.
func (p *Poller) loadCalibration(ctx context.Context, d *gmc.Device) (gmc.Calibration, error) {
	if spec := p.cfg.Poll.CalibrationOverride; spec != "" {
		cal, err := gmc.ParseCalibrationSpec(spec)
		if err != nil {
			return gmc.Calibration{}, fmt.Errorf("%sCALIBRATION: %w", config.EnvPrefix, err)
		}
		p.log.Info("using configured calibration override", "calibration", cal.String())
		return cal, nil
	}

	raw, err := d.Exchange(ctx, gmc.CmdGetConfig)
	if err != nil {
		return gmc.Calibration{}, fmt.Errorf("reading calibration from device: %w", err)
	}
	cal, err := gmc.ParseCalibration(raw)
	if err != nil {
		return gmc.Calibration{}, fmt.Errorf(
			"decoding the device calibration table: %w (set %sCALIBRATION to override it)",
			err, config.EnvPrefix)
	}
	return cal, nil
}

// closePort releases the current connection.
func (p *Poller) closePort() {
	p.mu.Lock()
	port := p.port
	p.port, p.device = nil, nil
	p.mu.Unlock()

	if port != nil {
		if err := closePort(port); err != nil {
			p.log.Warn("closing serial port", "error", err)
		}
	}
}

// closePort closes a port if it implements io.Closer. The Port interface does
// not require Close, because a fake in a test has nothing to release.
func closePort(port serialport.Port) error {
	if c, ok := port.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// isConnectionFailure reports whether an error suggests the link itself is
// gone, as opposed to one malformed reply.
//
// A short or desynchronized response is a data problem and the connection is
// still fine, so reconnecting on those would throw away a working port every
// few hundred reads. A read error from the device node is different: USB serial
// adapters do disappear and come back, and the only recovery is to reopen.
func isConnectionFailure(err error) bool {
	switch {
	case errors.Is(err, gmc.ErrUnexpectedLength),
		errors.Is(err, gmc.ErrDesynchronized),
		errors.Is(err, gmc.ErrBadTerminator),
		errors.Is(err, gmc.ErrNoResponse):
		return false
	default:
		return true
	}
}

// recordSuccess notes a good cycle.
func (p *Poller) recordSuccess(at time.Time) {
	p.mu.Lock()
	p.lastSuccess, p.lastError = at, ""
	p.mu.Unlock()
	if p.rec != nil {
		p.rec.RecordPollSuccess(at)
	}
}

// recordFailure notes a bad cycle.
func (p *Poller) recordFailure(reason string) {
	p.mu.Lock()
	p.lastError = reason
	p.mu.Unlock()
	if p.rec != nil {
		p.rec.RecordPollFailure()
	}
}

// Status is a snapshot of health, served by the health endpoints.
type Status struct {
	Connected     bool       `json:"connected"`
	LastSuccess   *time.Time `json:"lastSuccessfulRead,omitempty"`
	LastErrorText string     `json:"lastError,omitempty"`
	Device        DeviceInfo `json:"device"`
	Calibration   string     `json:"calibration,omitempty"`
}

// Status returns the current health snapshot.
func (p *Poller) Status() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()

	s := Status{
		Connected:     p.device != nil,
		LastErrorText: p.lastError,
		Device:        p.info,
	}
	if !p.lastSuccess.IsZero() {
		last := p.lastSuccess
		s.LastSuccess = &last
	}
	if len(p.calibration.Points) > 0 {
		s.Calibration = p.calibration.String()
	}
	return s
}

// SinceLastSuccess reports how long ago the last good reading was, and whether
// there has been one at all.
func (p *Poller) SinceLastSuccess() (time.Duration, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.lastSuccess.IsZero() {
		return 0, false
	}
	return time.Since(p.lastSuccess), true
}
