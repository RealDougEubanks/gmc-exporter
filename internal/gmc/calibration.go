package gmc

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Calibration layout inside the 256-byte <GETCFG>> block.
//
// GQ-RFC1201 section 7 documents only that GETCFG returns 256 bytes. It says
// nothing about the contents, so this layout was derived by measurement rather
// than from the specification, and it should be treated as a well-supported
// observation rather than a vendor guarantee.
//
// Method: 100 consecutive config blocks were captured from a GMC-320 (all 100
// byte-identical). Every offset in the block was tested as a candidate
// "uint16 followed by float32" pair. Exactly three offsets yielded a sane
// CPM/microsievert relationship, and all three agreed on the same ratio:
//
//	offset  8:   60 CPM -> 0.39 uSv/h
//	offset 14:  240 CPM -> 1.56 uSv/h
//	offset 20: 1000 CPM -> 6.50 uSv/h
//
// all three being exactly 0.0065 uSv/h per CPM. That was corroborated
// independently by observing the previously deployed exporter's own output,
// which reported 28 CPM as 0.18 uSv/h (28 x 0.0065 = 0.182).
//
// Note the mixed endianness, which is the detail most likely to be got wrong
// by assumption: the CPM value is big-endian, matching GETCPM in section 2,
// while the microsievert value is a little-endian IEEE-754 float32.
const (
	// calibrationOffset is where the first calibration entry begins.
	calibrationOffset = 8
	// calibrationEntrySize is 2 bytes of CPM plus 4 bytes of float32.
	calibrationEntrySize = 6
	// calibrationPoints is how many entries the block carries.
	calibrationPoints = 3
)

// CalibrationPoint maps a count rate to a dose rate.
type CalibrationPoint struct {
	// CPM is counts per minute.
	CPM uint16
	// MicroSievertsPerHour is the dose rate at that count rate.
	MicroSievertsPerHour float64
}

// Calibration is the device's CPM-to-dose conversion table, ordered by CPM.
type Calibration struct {
	Points []CalibrationPoint
}

// ParseCalibration extracts the calibration table from a <GETCFG>> block.
//
// Entries whose CPM is zero are skipped: an unprogrammed slot reads as zero and
// would otherwise divide by zero downstream.
func ParseCalibration(cfg []byte) (Calibration, error) {
	if err := checkLength(CmdGetConfig.Name, cfg, CmdGetConfig.ResponseBytes); err != nil {
		return Calibration{}, err
	}

	var points []CalibrationPoint
	for i := 0; i < calibrationPoints; i++ {
		off := calibrationOffset + i*calibrationEntrySize

		cpm := binary.BigEndian.Uint16(cfg[off : off+2])
		bits := binary.LittleEndian.Uint32(cfg[off+2 : off+6])
		usv := float64(math.Float32frombits(bits))

		if cpm == 0 {
			continue
		}
		if math.IsNaN(usv) || math.IsInf(usv, 0) || usv <= 0 {
			return Calibration{}, fmt.Errorf(
				"gmc: calibration entry %d at offset %d has CPM %d but a non-positive dose rate %v",
				i, off, cpm, usv)
		}
		points = append(points, CalibrationPoint{CPM: cpm, MicroSievertsPerHour: usv})
	}

	if len(points) == 0 {
		return Calibration{}, fmt.Errorf(
			"gmc: no usable calibration entries found in the %d-byte config block",
			len(cfg))
	}

	sort.Slice(points, func(i, j int) bool { return points[i].CPM < points[j].CPM })
	return Calibration{Points: points}, nil
}

// MicroSievertsPerHour converts a count rate to a dose rate.
//
// Between calibration points the conversion is linear. Below the lowest point
// it scales from the origin through that point, and above the highest it
// continues along the slope of the last segment. The device's own table is
// perfectly linear, so in practice every path here produces the same answer;
// the interpolation exists so a device calibrated with a non-linear table is
// still handled correctly.
func (c Calibration) MicroSievertsPerHour(cpm float64) float64 {
	if len(c.Points) == 0 || cpm <= 0 {
		return 0
	}

	first := c.Points[0]
	if cpm <= float64(first.CPM) {
		return cpm * (first.MicroSievertsPerHour / float64(first.CPM))
	}

	for i := 1; i < len(c.Points); i++ {
		lo, hi := c.Points[i-1], c.Points[i]
		if cpm > float64(hi.CPM) {
			continue
		}
		span := float64(hi.CPM - lo.CPM)
		if span == 0 {
			return hi.MicroSievertsPerHour
		}
		frac := (cpm - float64(lo.CPM)) / span
		return lo.MicroSievertsPerHour + frac*(hi.MicroSievertsPerHour-lo.MicroSievertsPerHour)
	}

	// Above the top of the table: extend the final segment's slope.
	last := c.Points[len(c.Points)-1]
	if len(c.Points) == 1 {
		return cpm * (last.MicroSievertsPerHour / float64(last.CPM))
	}
	prev := c.Points[len(c.Points)-2]
	span := float64(last.CPM - prev.CPM)
	if span == 0 {
		return last.MicroSievertsPerHour
	}
	slope := (last.MicroSievertsPerHour - prev.MicroSievertsPerHour) / span
	return last.MicroSievertsPerHour + (cpm-float64(last.CPM))*slope
}

// ParseCalibrationSpec builds a calibration table from a configuration string
// of comma-separated "cpm:usv" pairs, for example "60:0.39,240:1.56,1000:6.5".
//
// This exists because the layout of the configuration block is measured rather
// than specified. A unit whose firmware stores the table elsewhere, or a user
// who has calibrated against a reference source, can override it without
// waiting for a code change.
func ParseCalibrationSpec(spec string) (Calibration, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Calibration{}, fmt.Errorf("gmc: calibration spec is empty")
	}

	var points []CalibrationPoint
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		cpmText, usvText, found := strings.Cut(pair, ":")
		if !found {
			return Calibration{}, fmt.Errorf(
				"gmc: calibration entry %q is not in cpm:usv form", pair)
		}

		cpm, err := strconv.ParseUint(strings.TrimSpace(cpmText), 10, 16)
		if err != nil {
			return Calibration{}, fmt.Errorf(
				"gmc: calibration entry %q: %q is not a CPM value between 0 and 65535", pair, cpmText)
		}
		usv, err := strconv.ParseFloat(strings.TrimSpace(usvText), 64)
		if err != nil {
			return Calibration{}, fmt.Errorf(
				"gmc: calibration entry %q: %q is not a number", pair, usvText)
		}
		if cpm == 0 {
			return Calibration{}, fmt.Errorf("gmc: calibration entry %q has a CPM of zero", pair)
		}
		if usv <= 0 || math.IsNaN(usv) || math.IsInf(usv, 0) {
			return Calibration{}, fmt.Errorf(
				"gmc: calibration entry %q has a non-positive dose rate", pair)
		}
		points = append(points, CalibrationPoint{CPM: uint16(cpm), MicroSievertsPerHour: usv})
	}

	if len(points) == 0 {
		return Calibration{}, fmt.Errorf("gmc: calibration spec %q contained no entries", spec)
	}

	sort.Slice(points, func(i, j int) bool { return points[i].CPM < points[j].CPM })
	return Calibration{Points: points}, nil
}

// String renders the table for the startup log, so the calibration actually in
// use is visible in operation rather than assumed.
func (c Calibration) String() string {
	if len(c.Points) == 0 {
		return "(no calibration points)"
	}
	out := ""
	for i, p := range c.Points {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%d CPM=%.4g uSv/h", p.CPM, p.MicroSievertsPerHour)
	}
	return out
}
