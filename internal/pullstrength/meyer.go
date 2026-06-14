package pullstrength

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"wirebonding/ultra/pkg/models"
)

const (
	DefaultPullThresholdGrams = 5.0
	GravityMetersPerSec2      = 9.80665

	DefaultMaxwellTauS      = 25e-6
	DefaultMeyerHardnessGPa = 0.12
	DefaultBondAreaUm2      = 2500.0
	DefaultEnergyCoeff      = 1.85
	DefaultYieldCoeff       = 0.62

	sampleIntervalNs = 10_000
)

type BondingFeatureVector struct {
	TimestampNs   uint64
	SeqNo         uint32
	HardnessRatio float64
	DissipationJ  float64
	ContactArea   float64
	SofteningDeg  float64
	PhaseLagRad   float64
	EnergyRatio   float64
}

type PullStrengthPrediction struct {
	TimestampNs  uint64
	SeqNo        uint32
	PullGrams    float64
	ShearGrams   float64
	Confidence   float64
	BelowRedline bool
	Features     BondingFeatureVector
}

type MaxwellMeyerConfig struct {
	MaxwellTauS        float64
	MeyerHardnessGPa   float64
	BondAreaUm2        float64
	EnergyCoeff        float64
	YieldCoeff         float64
	RedlineGrams       float64
	BondingFreqHz      float64
	SofteningWindowNs  uint64
	EnergyWindowNs     uint64
}

func DefaultMeyerConfig() MaxwellMeyerConfig {
	return MaxwellMeyerConfig{
		MaxwellTauS:       DefaultMaxwellTauS,
		MeyerHardnessGPa:  DefaultMeyerHardnessGPa,
		BondAreaUm2:       DefaultBondAreaUm2,
		EnergyCoeff:       DefaultEnergyCoeff,
		YieldCoeff:        DefaultYieldCoeff,
		RedlineGrams:      DefaultPullThresholdGrams,
		BondingFreqHz:     40_000.0,
		SofteningWindowNs: 500 * 1000,
		EnergyWindowNs:    2 * 1000 * 1000,
	}
}

type MeyerPredictor struct {
	mu sync.RWMutex

	cfg    MaxwellMeyerConfig
	latest PullStrengthPrediction

	softBuf  *softeningTracker
	energyBuf *energyTracker

	totalPredictions atomic.Uint64
	redlineHits      atomic.Uint64

	lastTsNs uint64
	lastZ    complex128
	started  bool
}

type softeningTracker struct {
	capacity int
	idx      int
	full     bool
	rows     []softeningRow
}

type softeningRow struct {
	tsNs    uint64
	hardRat float64
	phase   float64
}

type energyTracker struct {
	capacity int
	idx      int
	full     bool
	rows     []energyRow
	accJ     float64
}

type energyRow struct {
	tsNs uint64
	vI   float64
	qI   float64
}

func NewMeyerPredictor(cfg MaxwellMeyerConfig) *MeyerPredictor {
	if cfg.MaxwellTauS <= 0 {
		cfg.MaxwellTauS = DefaultMaxwellTauS
	}
	if cfg.MeyerHardnessGPa <= 0 {
		cfg.MeyerHardnessGPa = DefaultMeyerHardnessGPa
	}
	if cfg.BondAreaUm2 <= 0 {
		cfg.BondAreaUm2 = DefaultBondAreaUm2
	}
	if cfg.EnergyCoeff <= 0 {
		cfg.EnergyCoeff = DefaultEnergyCoeff
	}
	if cfg.YieldCoeff <= 0 {
		cfg.YieldCoeff = DefaultYieldCoeff
	}
	if cfg.RedlineGrams <= 0 {
		cfg.RedlineGrams = DefaultPullThresholdGrams
	}
	if cfg.BondingFreqHz <= 0 {
		cfg.BondingFreqHz = 40_000.0
	}
	if cfg.SofteningWindowNs == 0 {
		cfg.SofteningWindowNs = 500 * 1000
	}
	if cfg.EnergyWindowNs == 0 {
		cfg.EnergyWindowNs = 2 * 1000 * 1000
	}

	softSamples := int(cfg.SofteningWindowNs / sampleIntervalNs)
	if softSamples < 16 {
		softSamples = 16
	}
	energySamples := int(cfg.EnergyWindowNs / sampleIntervalNs)
	if energySamples < 64 {
		energySamples = 64
	}

	return &MeyerPredictor{
		cfg:       cfg,
		softBuf:   newSofteningTracker(softSamples),
		energyBuf: newEnergyTracker(energySamples),
	}
}

func newSofteningTracker(n int) *softeningTracker {
	return &softeningTracker{
		capacity: n,
		rows:     make([]softeningRow, n),
	}
}

func (s *softeningTracker) push(tsNs uint64, hardRat, phase float64) {
	s.rows[s.idx] = softeningRow{tsNs, hardRat, phase}
	s.idx = (s.idx + 1) % s.capacity
	if s.idx == 0 {
		s.full = true
	}
}

func (s *softeningTracker) stats(windowNs uint64, nowNs uint64) (avgHard, avgPhase, degSoftening float64, ok bool) {
	valid := 0
	var sumH, sumP float64
	var minH, maxH float64
	first := true
	limit := s.capacity
	if !s.full {
		limit = s.idx
	}
	for i := 0; i < limit; i++ {
		r := s.rows[i]
		if r.tsNs == 0 {
			continue
		}
		if nowNs > 0 && nowNs-r.tsNs > windowNs {
			continue
		}
		valid++
		sumH += r.hardRat
		sumP += r.phase
		if first || r.hardRat < minH {
			minH = r.hardRat
		}
		if first || r.hardRat > maxH {
			maxH = r.hardRat
		}
		first = false
	}
	if valid < 4 {
		return 0, 0, 0, false
	}
	avgHard = sumH / float64(valid)
	avgPhase = sumP / float64(valid)
	if maxH > 1e-9 {
		degSoftening = (maxH - minH) / maxH
	}
	return avgHard, avgPhase, degSoftening, true
}

func newEnergyTracker(n int) *energyTracker {
	return &energyTracker{
		capacity: n,
		rows:     make([]energyRow, n),
	}
}

func (e *energyTracker) push(tsNs uint64, vI, qI float64, dtNs float64) {
	instPow := math.Hypot(vI, qI)
	addJ := instPow * (dtNs * 1e-9)
	old := e.rows[e.idx]
	if old.tsNs > 0 {
		oldPow := math.Hypot(old.vI, old.qI)
		subJ := oldPow * (dtNs * 1e-9)
		e.accJ -= subJ
	}
	e.accJ += addJ
	if e.accJ < 0 {
		e.accJ = 0
	}
	e.rows[e.idx] = energyRow{tsNs, vI, qI}
	e.idx = (e.idx + 1) % e.capacity
	if e.idx == 0 {
		e.full = true
	}
}

func (e *energyTracker) accumulatedJoules() float64 {
	return e.accJ
}

func (mp *MeyerPredictor) PushPhasor(seqNo uint32, tsNs uint64,
	vPh, iPh models.Phasor, zp models.ImpedancePoint) (pred *PullStrengthPrediction, ready bool) {

	mp.mu.Lock()
	defer mp.mu.Unlock()

	dtNs := float64(sampleIntervalNs)
	if mp.lastTsNs > 0 && tsNs > mp.lastTsNs {
		dtNs = float64(tsNs - mp.lastTsNs)
	}
	mp.lastTsNs = tsNs

	iMagSq := iPh.Real*iPh.Real + iPh.Imag*iPh.Imag
	if iMagSq < 1e-12 {
		return nil, false
	}
	instVI := vPh.Real*iPh.Real + vPh.Imag*iPh.Imag
	instQI := vPh.Imag*iPh.Real - vPh.Real*iPh.Imag
	mp.energyBuf.push(tsNs, instVI, instQI, dtNs)

	zMag := zp.Magnitude
	if zMag < 1e-9 {
		return nil, false
	}
	phaseLag := zp.Phase
	hardRatio := zMag

	mp.softBuf.push(tsNs, hardRatio, phaseLag)

	avgHard, avgPhase, degSoft, softOk := mp.softBuf.stats(mp.cfg.SofteningWindowNs, tsNs)
	if !softOk {
		return nil, false
	}
	dissJ := mp.energyBuf.accumulatedJoules()

	feat := BondingFeatureVector{
		TimestampNs:   tsNs,
		SeqNo:         seqNo,
		HardnessRatio: avgHard,
		DissipationJ:  dissJ,
		ContactArea:   mp.cfg.BondAreaUm2,
		SofteningDeg:  degSoft,
		PhaseLagRad:   avgPhase,
		EnergyRatio:   0,
	}
	if dissJ > 1e-12 {
		feat.EnergyRatio = dissJ / (1.0 + avgHard*1e-3)
	}

	pullGrams, shearGrams, conf := mp.solveMaxwellMeyer(feat)
	below := pullGrams < mp.cfg.RedlineGrams

	result := PullStrengthPrediction{
		TimestampNs:  tsNs,
		SeqNo:        seqNo,
		PullGrams:    pullGrams,
		ShearGrams:   shearGrams,
		Confidence:   conf,
		BelowRedline: below,
		Features:     feat,
	}
	mp.latest = result
	mp.totalPredictions.Add(1)
	if below {
		mp.redlineHits.Add(1)
	}
	return &result, true
}

func (mp *MeyerPredictor) solveMaxwellMeyer(f BondingFeatureVector) (pullG, shearG, confidence float64) {
	cfg := mp.cfg

	tau := cfg.MaxwellTauS
	omega := 2.0 * math.Pi * cfg.BondingFreqHz

	cosPhase := math.Cos(f.PhaseLagRad)
	sinPhase := math.Abs(math.Sin(f.PhaseLagRad))

	dynModGPa := cfg.MeyerHardnessGPa / (1.0 + 1e-3*math.Abs(f.HardnessRatio))

	softFactor := 1.0 - cfg.YieldCoeff*f.SofteningDeg
	if softFactor < 0.1 {
		softFactor = 0.1
	}

	etaTerm := omega * tau / math.Sqrt(1.0+(omega*tau)*(omega*tau))
	lossModGPa := dynModGPa * etaTerm * sinPhase
	storageModGPa := dynModGPa * (1.0 - etaTerm*sinPhase)

	if storageModGPa < 0.01 {
		storageModGPa = 0.01
	}

	areaM2 := f.ContactArea * 1e-12
	pullPa := storageModGPa * 1e9 * softFactor * cosPhase * f.EnergyRatio
	shearPa := lossModGPa * 1e9 * softFactor * (1.0 - cosPhase)

	minPa := 100_000.0
	if pullPa < minPa {
		pullPa = minPa + cfg.EnergyCoeff*math.Sqrt(f.DissipationJ+1e-9)*1e6
	}
	if shearPa < minPa {
		shearPa = minPa + cfg.EnergyCoeff*math.Sqrt(f.DissipationJ+1e-9)*0.8e6
	}

	pullN := pullPa * areaM2
	shearN := shearPa * areaM2

	pullG = (pullN / GravityMetersPerSec2) * 1000.0
	shearG = (shearN / GravityMetersPerSec2) * 1000.0

	if pullG < 0.5 {
		pullG = 0.5
	}
	if shearG < 0.5 {
		shearG = 0.5
	}

	confidence = 1.0 / (1.0 + math.Exp(-(f.SofteningDeg-0.15)*8.0))
	if confidence < 0.2 {
		confidence = 0.2
	}
	if confidence > 0.99 {
		confidence = 0.99
	}
	return pullG, shearG, confidence
}

func (mp *MeyerPredictor) Latest() (PullStrengthPrediction, bool) {
	mp.mu.RLock()
	defer mp.mu.RUnlock()
	if !mp.started && mp.totalPredictions.Load() == 0 {
		return PullStrengthPrediction{}, false
	}
	return mp.latest, true
}

func (mp *MeyerPredictor) TotalPredictions() uint64 { return mp.totalPredictions.Load() }
func (mp *MeyerPredictor) RedlineHits() uint64      { return mp.redlineHits.Load() }
func (mp *MeyerPredictor) RedlineGrams() float64    { return mp.cfg.RedlineGrams }

func (mp *MeyerPredictor) SetRedline(grams float64) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if grams > 0 {
		mp.cfg.RedlineGrams = grams
	}
}

func (mp *MeyerPredictor) Reset() {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.latest = PullStrengthPrediction{}
	mp.softBuf = newSofteningTracker(mp.softBuf.capacity)
	mp.energyBuf = newEnergyTracker(mp.energyBuf.capacity)
	mp.totalPredictions.Store(0)
	mp.redlineHits.Store(0)
	mp.lastTsNs = 0
	mp.lastZ = 0
	mp.started = false
}

type PullEvent struct {
	ChannelID    string
	Prediction   PullStrengthPrediction
	EventID      uint64
	TriggeredAt  time.Time
}
