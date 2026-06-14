package dsp

import (
	"math"
	"sync"

	"wirebonding/ultra/pkg/models"
)

const (
	DefaultWindowSize    = 512
	DefaultSlideStep     = 1
	DefaultFundamentalHz = 40_000.0
)

type WindowType int

const (
	WindowRectangular WindowType = iota
	WindowHann
	WindowHamming
	WindowBlackman
)

type SlidingDFT struct {
	mu             sync.RWMutex
	windowSize     int
	targetFreqHz   float64
	sampleRateHz   float64
	k              int

	cosTable       []float64
	sinTable       []float64
	windowCoeff    []float64
	weightedCos    []float64
	weightedSin    []float64

	sampleBuf      []float64
	bufHead        int
	filled         bool
	sampleCount    uint64

	coherentGain   float64
}

type PhasorResult struct {
	Real         float64
	Imag         float64
	Magnitude    float64
	Angle        float64
	FreqHz       float64
	TimeNs       uint64
	Ready        bool
	WindowSample int
}

func NewSlidingDFT(windowSize int, targetFreqHz float64, sampleRateHz float64, winType WindowType) *SlidingDFT {
	if windowSize <= 0 {
		windowSize = DefaultWindowSize
	}
	if sampleRateHz <= 0 {
		sampleRateHz = float64(models.SampleRateHz)
	}
	if targetFreqHz <= 0 {
		targetFreqHz = DefaultFundamentalHz
	}
	k := int(math.Round(targetFreqHz * float64(windowSize) / sampleRateHz))
	if k < 1 {
		k = 1
	}
	if k >= windowSize/2 {
		k = windowSize/2 - 1
	}

	sd := &SlidingDFT{
		windowSize:   windowSize,
		targetFreqHz: targetFreqHz,
		sampleRateHz: sampleRateHz,
		k:            k,
		sampleBuf:    make([]float64, windowSize),
		windowCoeff:  make([]float64, windowSize),
		cosTable:     make([]float64, windowSize),
		sinTable:     make([]float64, windowSize),
		weightedCos:  make([]float64, windowSize),
		weightedSin:  make([]float64, windowSize),
	}

	buildWindow(sd.windowCoeff, winType)
	winSum := 0.0
	for n := 0; n < windowSize; n++ {
		w := sd.windowCoeff[n]
		phase := 2.0 * math.Pi * float64(k*n) / float64(windowSize)
		c := math.Cos(phase)
		s := math.Sin(phase)
		sd.cosTable[n] = c
		sd.sinTable[n] = s
		sd.weightedCos[n] = w * c
		sd.weightedSin[n] = w * s
		winSum += w
	}
	sd.coherentGain = 2.0 / winSum
	return sd
}

func buildWindow(out []float64, wt WindowType) {
	N := len(out)
	if N == 0 {
		return
	}
	switch wt {
	case WindowRectangular:
		for i := range out {
			out[i] = 1.0
		}
	case WindowHann:
		for i := range out {
			out[i] = 0.5 * (1.0 - math.Cos(2.0*math.Pi*float64(i)/float64(N-1)))
		}
	case WindowHamming:
		for i := range out {
			out[i] = 0.54 - 0.46*math.Cos(2.0*math.Pi*float64(i)/float64(N-1))
		}
	case WindowBlackman:
		for i := range out {
			x := 2.0 * math.Pi * float64(i) / float64(N-1)
			out[i] = 0.42 - 0.5*math.Cos(x) + 0.08*math.Cos(2.0*x)
		}
	}
}

func (sd *SlidingDFT) Reset() {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	for i := range sd.sampleBuf {
		sd.sampleBuf[i] = 0
	}
	sd.bufHead = 0
	sd.filled = false
	sd.sampleCount = 0
}

func (sd *SlidingDFT) Update(sample float64, timeNs uint64) PhasorResult {
	sd.mu.Lock()
	defer sd.mu.Unlock()

	N := sd.windowSize
	sd.sampleBuf[sd.bufHead] = sample
	sd.bufHead = (sd.bufHead + 1) % N
	if sd.bufHead == 0 {
		sd.filled = true
	}
	sd.sampleCount++

	var result PhasorResult
	result.TimeNs = timeNs
	result.WindowSample = int(sd.sampleCount % uint64(N))
	if !sd.filled {
		result.Ready = false
		return result
	}

	head := sd.bufHead
	real := 0.0
	imag := 0.0
	for n := 0; n < N; n++ {
		sidx := (head + n) % N
		x := sd.sampleBuf[sidx]
		real += x * sd.weightedCos[n]
		imag -= x * sd.weightedSin[n]
	}

	real *= sd.coherentGain
	imag *= sd.coherentGain
	mag := math.Sqrt(real*real + imag*imag)
	angle := math.Atan2(imag, real)

	result.Real = real
	result.Imag = imag
	result.Magnitude = mag
	result.Angle = angle
	result.FreqHz = float64(sd.k) * sd.sampleRateHz / float64(N)
	result.Ready = true
	return result
}

func (sd *SlidingDFT) UpdateBatch(samples []float64, startTimeNs uint64, results []PhasorResult) int {
	if len(samples) == 0 {
		return 0
	}
	count := 0
	interval := uint64(models.SampleIntervalNs)
	for i, s := range samples {
		ts := startTimeNs + uint64(i)*interval
		res := sd.Update(s, ts)
		if i < len(results) {
			results[i] = res
			if res.Ready {
				count++
			}
		}
	}
	return count
}

func (sd *SlidingDFT) WindowSize() int      { return sd.windowSize }
func (sd *SlidingDFT) TargetBin() int       { return sd.k }
func (sd *SlidingDFT) EffectiveFrequency() float64 {
	return float64(sd.k) * sd.sampleRateHz / float64(sd.windowSize)
}
