package dsp

import (
	"math"
	"sync"

	"wirebonding/ultra/pkg/models"
)

type DualPhasorPipeline struct {
	mu          sync.Mutex
	voltageDFT  *SlidingDFT
	currentDFT  *SlidingDFT
	windowSize  int
	sampleIdx   uint64
	lastVoltPh  *PhasorResult
	lastCurrPh  *PhasorResult
	trackedFreq float64
	freqLock    bool
	freqAlpha   float64
}

func NewDualPhasorPipeline(windowSize int, targetFreqHz float64, sampleRateHz float64) *DualPhasorPipeline {
	wdft := NewSlidingDFT(windowSize, targetFreqHz, sampleRateHz, WindowHann)
	idft := NewSlidingDFT(windowSize, targetFreqHz, sampleRateHz, WindowHann)
	return &DualPhasorPipeline{
		voltageDFT:  wdft,
		currentDFT:  idft,
		windowSize:  windowSize,
		trackedFreq: targetFreqHz,
		freqAlpha:   0.125,
	}
}

func (dp *DualPhasorPipeline) Reset() {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	dp.voltageDFT.Reset()
	dp.currentDFT.Reset()
	dp.sampleIdx = 0
	dp.lastVoltPh = nil
	dp.lastCurrPh = nil
}

func (dp *DualPhasorPipeline) ProcessSample(sample models.WaveSample) (voltage, current *models.Phasor, ready bool) {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	dp.sampleIdx++

	vRes := dp.voltageDFT.Update(sample.Voltage, sample.TimestampNs)
	iRes := dp.currentDFT.Update(sample.Current, sample.TimestampNs)

	if !vRes.Ready || !iRes.Ready {
		return nil, nil, false
	}

	dp.lastVoltPh = &vRes
	dp.lastCurrPh = &iRes

	if !dp.freqLock && dp.lastVoltPh != nil && dp.lastCurrPh != nil {
		dp.trackedFreq = dp.trackedFreq*(1.0-dp.freqAlpha) + vRes.FreqHz*dp.freqAlpha
	}

	vPhasor := &models.Phasor{
		Real:      vRes.Real,
		Imag:      vRes.Imag,
		Magnitude: vRes.Magnitude,
		Angle:     vRes.Angle,
		FreqHz:    dp.trackedFreq,
	}
	iPhasor := &models.Phasor{
		Real:      iRes.Real,
		Imag:      iRes.Imag,
		Magnitude: iRes.Magnitude,
		Angle:     iRes.Angle,
		FreqHz:    dp.trackedFreq,
	}
	return vPhasor, iPhasor, true
}

func (dp *DualPhasorPipeline) ProcessBatch(samples []models.WaveSample,
	voltOut []models.Phasor, currOut []models.Phasor) int {
	readyCount := 0
	for i, s := range samples {
		v, c, ok := dp.ProcessSample(s)
		if ok && i < len(voltOut) && i < len(currOut) {
			voltOut[i] = *v
			currOut[i] = *c
			readyCount++
		}
	}
	return readyCount
}

type Goertzel struct {
	TargetFreq   float64
	SampleRate   float64
	N            int
	coef         float64
	count        int
	s1           float64
	s2           float64
}

func NewGoertzel(targetFreqHz float64, sampleRateHz float64, n int) *Goertzel {
	if n <= 0 {
		n = DefaultWindowSize
	}
	if sampleRateHz <= 0 {
		sampleRateHz = float64(models.SampleRateHz)
	}
	k := math.Round(targetFreqHz * float64(n) / sampleRateHz)
	omega := 2.0 * math.Pi * k / float64(n)
	return &Goertzel{
		TargetFreq: targetFreqHz,
		SampleRate: sampleRateHz,
		N:          n,
		coef:       2.0 * math.Cos(omega),
	}
}

func (g *Goertzel) Reset() {
	g.count = 0
	g.s1 = 0
	g.s2 = 0
}

func (g *Goertzel) AddSample(x float64) {
	y := x + g.coef*g.s1 - g.s2
	g.s2 = g.s1
	g.s1 = y
	g.count++
}

func (g *Goertzel) AddBatch(xs []float64) {
	for _, x := range xs {
		g.AddSample(x)
	}
}

type GoertzelResult struct {
	Real      float64
	Imag      float64
	Magnitude float64
	Angle     float64
	FreqHz    float64
	Complete  bool
}

func (g *Goertzel) MagnitudeSquared() float64 {
	return g.s1*g.s1 + g.s2*g.s2 - g.coef*g.s1*g.s2
}

func (g *Goertzel) Result() GoertzelResult {
	if g.count < g.N {
		return GoertzelResult{Complete: false}
	}
	real := 0.5 * g.coef * g.s1 - g.s2
	imag := math.Sqrt(1.0 - 0.25*g.coef*g.coef) * g.s1
	mag := math.Sqrt(real*real + imag*imag)
	ang := math.Atan2(imag, real)
	norm := 2.0 / float64(g.N)
	return GoertzelResult{
		Real:      real * norm,
		Imag:      imag * norm,
		Magnitude: mag * norm,
		Angle:     ang,
		FreqHz:    g.TargetFreq,
		Complete:  true,
	}
}
