package impedance

import (
	"math"
	"sync"
	"time"

	"wirebonding/ultra/pkg/models"
)

const (
	DefaultMatrixRows = 2048
	DefaultMatrixCols = 8
	DefaultBondingHz  = 40_000.0
)

type MatrixCol int

const (
	ColTimeNs MatrixCol = iota
	ColSeqNo
	ColVmag
	ColImag
	ColZreal
	ColZimag
	ColZmag
	ColZphase
	NumCols
)

type SpatiotemporalMatrix struct {
	mu         sync.RWMutex
	rows       int
	writeIdx   int
	full       bool
	timestamps []uint64
	seqNos     []uint32
	vmag       []float64
	imagCol    []float64
	zreal      []float64
	zimag      []float64
	zmag       []float64
	zphase     []float64

	baseline   struct {
		z0Real   float64
		z0Imag   float64
		z0Mag    float64
		locked   bool
		samples  int
	}

	anomaly struct {
		zJumpThreshold   float64
		phaseThreshold   float64
		alarmCount       uint64
		lastAlarmIdx     int
	}

	channelID    string
	bondingFreq  float64
	smoothAlpha  float64
	lastZ        complex128
}

func NewSpatiotemporalMatrix(rows int, channelID string) *SpatiotemporalMatrix {
	if rows <= 0 {
		rows = DefaultMatrixRows
	}
	m := &SpatiotemporalMatrix{
		rows:         rows,
		timestamps:   make([]uint64, rows),
		seqNos:       make([]uint32, rows),
		vmag:         make([]float64, rows),
		imagCol:      make([]float64, rows),
		zreal:        make([]float64, rows),
		zimag:        make([]float64, rows),
		zmag:         make([]float64, rows),
		zphase:       make([]float64, rows),
		channelID:    channelID,
		bondingFreq:  DefaultBondingHz,
		smoothAlpha:  0.0625,
	}
	m.anomaly.zJumpThreshold = 0.15
	m.anomaly.phaseThreshold = 12.0 * math.Pi / 180.0
	return m
}

func (m *SpatiotemporalMatrix) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeIdx = 0
	m.full = false
	for i := 0; i < m.rows; i++ {
		m.timestamps[i] = 0
		m.seqNos[i] = 0
		m.vmag[i] = 0
		m.imagCol[i] = 0
		m.zreal[i] = 0
		m.zimag[i] = 0
		m.zmag[i] = 0
		m.zphase[i] = 0
	}
	m.baseline.locked = false
	m.baseline.samples = 0
	m.anomaly.alarmCount = 0
	m.lastZ = 0
}

func (m *SpatiotemporalMatrix) SolveImpedance(vPh, iPh models.Phasor) (models.ImpedancePoint, bool) {
	var z models.ImpedancePoint
	iMagSq := iPh.Real*iPh.Real + iPh.Imag*iPh.Imag
	if iMagSq < 1e-12 {
		return z, false
	}
	iInvDenom := 1.0 / iMagSq
	iv := vPh.Real*iPh.Real + vPh.Imag*iPh.Imag
	iq := vPh.Imag*iPh.Real - vPh.Real*iPh.Imag
	zr := iv * iInvDenom
	zi := iq * iInvDenom

	if m.lastZ != 0 {
		prevR := real(m.lastZ)
		prevI := imag(m.lastZ)
		zr = prevR + m.smoothAlpha*(zr-prevR)
		zi = prevI + m.smoothAlpha*(zi-prevI)
	}
	m.lastZ = complex(zr, zi)

	z.Magnitude = math.Sqrt(zr*zr + zi*zi)
	if z.Magnitude < 1e-15 {
		return z, false
	}
	z.Resistance = zr
	z.Reactance = zi
	z.Phase = math.Atan2(zi, zr)

	if !m.baseline.locked {
		m.baseline.z0Real += zr
		m.baseline.z0Imag += zi
		m.baseline.z0Mag += z.Magnitude
		m.baseline.samples++
		if m.baseline.samples >= 256 {
			invN := 1.0 / float64(m.baseline.samples)
			m.baseline.z0Real *= invN
			m.baseline.z0Imag *= invN
			m.baseline.z0Mag *= invN
			m.baseline.locked = true
		}
	}
	return z, true
}

type AnomalyEvent struct {
	ChannelID       string
	TimestampNs     uint64
	SeqNo           uint32
	DeviationPct    float64
	PhaseShiftDeg   float64
	ZBaseline       complex128
	ZMeasured       complex128
	AlarmID         uint64
}

func (m *SpatiotemporalMatrix) AppendRow(seqNo uint32, timestampNs uint64,
	vPh, iPh models.Phasor) (zp models.ImpedancePoint, anomaly *AnomalyEvent, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	z, solvable := m.SolveImpedance(vPh, iPh)
	if !solvable {
		return zp, nil, false
	}
	z.SeqNo = seqNo
	zp = z

	idx := m.writeIdx
	m.timestamps[idx] = timestampNs
	m.seqNos[idx] = seqNo
	m.vmag[idx] = vPh.Magnitude
	m.imagCol[idx] = iPh.Magnitude
	m.zreal[idx] = z.Resistance
	m.zimag[idx] = z.Reactance
	m.zmag[idx] = z.Magnitude
	m.zphase[idx] = z.Phase
	zp.TimestampNs = timestampNs

	m.writeIdx = (idx + 1) % m.rows
	if m.writeIdx == 0 {
		m.full = true
	}

	if m.baseline.locked {
		deltaR := (z.Magnitude - m.baseline.z0Mag) / (m.baseline.z0Mag + 1e-18)
		deltaPhase := math.Abs(z.Phase - math.Atan2(m.baseline.z0Imag, m.baseline.z0Real))
		if math.Abs(deltaR) > m.anomaly.zJumpThreshold || deltaPhase > m.anomaly.phaseThreshold {
			if idx != m.anomaly.lastAlarmIdx {
				m.anomaly.alarmCount++
				m.anomaly.lastAlarmIdx = idx
				anomaly = &AnomalyEvent{
					ChannelID:     m.channelID,
					TimestampNs:   timestampNs,
					SeqNo:         seqNo,
					DeviationPct:  deltaR * 100.0,
					PhaseShiftDeg: deltaPhase * 180.0 / math.Pi,
					ZBaseline:     complex(m.baseline.z0Real, m.baseline.z0Imag),
					ZMeasured:     complex(z.Resistance, z.Reactance),
					AlarmID:       m.anomaly.alarmCount,
				}
			}
		}
	}
	return zp, anomaly, true
}

func (m *SpatiotemporalMatrix) LatestRow() (z models.ImpedancePoint, vmag, imag float64, ts uint64, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	idx := m.writeIdx - 1
	if idx < 0 {
		idx = m.rows - 1
	}
	if !m.full && m.writeIdx == 0 {
		return z, 0, 0, 0, false
	}
	z.Resistance = m.zreal[idx]
	z.Reactance = m.zimag[idx]
	z.Magnitude = m.zmag[idx]
	z.Phase = m.zphase[idx]
	z.SeqNo = m.seqNos[idx]
	z.TimestampNs = m.timestamps[idx]
	return z, m.vmag[idx], m.imagCol[idx], m.timestamps[idx], true
}

type MatrixSnapshot struct {
	ChannelID      string
	Rows           int
	Full           bool
	TimestampsNs   []uint64
	ZReal          []float64
	ZImag          []float64
	ZMag           []float64
	ZPhase         []float64
	BaselineLocked bool
	BaselineZ      complex128
	AlarmCount     uint64
	GeneratedAt    time.Time
}

func (m *SpatiotemporalMatrix) SnapshotRecent(lastN int) MatrixSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	total := m.rows
	if m.full {
		total = m.rows
	} else {
		total = m.writeIdx
	}
	if lastN <= 0 || lastN > total {
		lastN = total
	}
	snap := MatrixSnapshot{
		ChannelID:      m.channelID,
		Rows:           lastN,
		Full:           m.full,
		TimestampsNs:   make([]uint64, lastN),
		ZReal:          make([]float64, lastN),
		ZImag:          make([]float64, lastN),
		ZMag:           make([]float64, lastN),
		ZPhase:         make([]float64, lastN),
		BaselineLocked: m.baseline.locked,
		BaselineZ:      complex(m.baseline.z0Real, m.baseline.z0Imag),
		AlarmCount:     m.anomaly.alarmCount,
		GeneratedAt:    time.Now(),
	}
	end := m.writeIdx
	start := end - lastN
	if start < 0 {
		start += m.rows
	}
	for i := 0; i < lastN; i++ {
		idx := (start + i) % m.rows
		snap.TimestampsNs[i] = m.timestamps[idx]
		snap.ZReal[i] = m.zreal[idx]
		snap.ZImag[i] = m.zimag[idx]
		snap.ZMag[i] = m.zmag[idx]
		snap.ZPhase[i] = m.zphase[idx]
	}
	return snap
}

func (m *SpatiotemporalMatrix) ChannelID() string { return m.channelID }
func (m *SpatiotemporalMatrix) IsFull() bool       { m.mu.RLock(); defer m.mu.RUnlock(); return m.full }
func (m *SpatiotemporalMatrix) BaselineLocked() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.baseline.locked
}
func (m *SpatiotemporalMatrix) AlarmCount() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.anomaly.alarmCount
}
func (m *SpatiotemporalMatrix) SetJumpThreshold(pct float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.anomaly.zJumpThreshold = pct / 100.0
}
func (m *SpatiotemporalMatrix) SetPhaseThreshold(deg float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.anomaly.phaseThreshold = deg * math.Pi / 180.0
}
