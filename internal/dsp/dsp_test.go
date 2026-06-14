package dsp

import (
	"math"
	"testing"

	"wirebonding/ultra/pkg/models"
)

func generateSineWave(n int, freqHz float64, sampleRate float64, amplitude float64, phase float64) []float64 {
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		t := float64(i) / sampleRate
		out[i] = amplitude * math.Sin(2.0*math.Pi*freqHz*t+phase)
	}
	return out
}

func TestSlidingDFT_OutputMagnitude(t *testing.T) {
	const (
		N          = 1024
		window     = 256
		targetHz   = 40_000.0
		sampleRate = 100_000.0
		amplitude  = 511.0
	)
	sig := generateSineWave(N, targetHz, sampleRate, amplitude, 0)
	sd := NewSlidingDFT(window, targetHz, sampleRate, WindowHann)
	mags := make([]float64, 0, N)
	for i := 0; i < N; i++ {
		res := sd.Update(sig[i], uint64(i)*models.SampleIntervalNs)
		if res.Ready {
			mags = append(mags, res.Magnitude)
		}
	}
	if len(mags) == 0 {
		t.Fatal("no ready DFT outputs")
	}
	lastN := mags[len(mags)/2:]
	avg := 0.0
	for _, m := range lastN {
		avg += m
	}
	avg /= float64(len(lastN))
	expected := amplitude
	tol := expected * 0.15
	if math.Abs(avg-expected) > tol {
		t.Errorf("magnitude mismatch: avg=%.4f expected≈%.4f (tol=±%.4f)", avg, expected, tol)
	}
}

func TestSlidingDFT_WarmupCount(t *testing.T) {
	N := 10000
	window := 512
	sig := generateSineWave(N, 40000, 100000, 500, 0)
	sd := NewSlidingDFT(window, 40000, 100000, WindowHann)
	firstReady := -1
	for i := 0; i < N; i++ {
		res := sd.Update(sig[i], 0)
		if res.Ready && firstReady == -1 {
			firstReady = i
			break
		}
	}
	if firstReady < 0 {
		t.Fatal("DFT never became ready")
	}
	if firstReady+1 < window {
		t.Errorf("DFT became ready too early: idx=%d window=%d", firstReady, window)
	}
}

func TestDualPhasorPipeline(t *testing.T) {
	N := 4096
	window := 512
	volt := generateSineWave(N, 40000, 100000, 512, 0)
	curr := generateSineWave(N, 40000, 100000, 128, math.Pi/4)
	samples := make([]models.WaveSample, N)
	for i := 0; i < N; i++ {
		samples[i] = models.WaveSample{
			TimestampNs: uint64(i) * 10000,
			Voltage:     volt[i],
			Current:     curr[i],
			SeqNo:       uint32(i),
		}
	}
	dp := NewDualPhasorPipeline(window, 40000, 100000)
	vPhasors := make([]models.Phasor, N)
	iPhasors := make([]models.Phasor, N)
	ready := dp.ProcessBatch(samples, vPhasors, iPhasors)
	if ready == 0 {
		t.Fatal("no ready phasor outputs")
	}
}

func TestGoertzel(t *testing.T) {
	N := 1024
	targetHz := 40000.0
	sampleRate := 100000.0
	sig := generateSineWave(N, targetHz, sampleRate, 512, 0.5)
	g := NewGoertzel(targetHz, sampleRate, N)
	g.AddBatch(sig)
	res := g.Result()
	if !res.Complete {
		t.Fatal("Goertzel result not complete")
	}
	expectedMag := 512.0
	if math.Abs(res.Magnitude-expectedMag) > expectedMag*0.3 {
		t.Errorf("Goertzel magnitude: got %.4f expected≈%.4f", res.Magnitude, expectedMag)
	}
}

func BenchmarkSlidingDFT(b *testing.B) {
	window := 512
	sampleRate := 100000.0
	targetHz := 40000.0
	N := 65536
	sig := generateSineWave(N, targetHz, sampleRate, 512, 0)
	sd := NewSlidingDFT(window, targetHz, sampleRate, WindowHann)
	for i := 0; i < window; i++ {
		sd.Update(sig[i], 0)
	}
	b.ResetTimer()
	b.ReportAllocs()
	idx := window
	for i := 0; i < b.N; i++ {
		sd.Update(sig[idx], 0)
		idx++
		if idx >= N {
			idx = window
		}
	}
}

func BenchmarkSlidingDFTBatch(b *testing.B) {
	window := 512
	sampleRate := 100000.0
	targetHz := 40000.0
	batchSize := 8192
	sig := generateSineWave(batchSize+window, targetHz, sampleRate, 512, 0)
	sd := NewSlidingDFT(window, targetHz, sampleRate, WindowHann)
	results := make([]PhasorResult, batchSize)
	for i := 0; i < window; i++ {
		sd.Update(sig[i], 0)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		n := sd.UpdateBatch(sig[window:window+batchSize], 0, results)
		_ = n
	}
}

func BenchmarkDualPhasor(b *testing.B) {
	window := 512
	targetHz := 40000.0
	sampleRate := 100000.0
	batchSize := 10000
	volt := generateSineWave(batchSize+window, targetHz, sampleRate, 512, 0)
	curr := generateSineWave(batchSize+window, targetHz, sampleRate, 128, 0.785)
	samples := make([]models.WaveSample, batchSize)
	for i := 0; i < batchSize; i++ {
		samples[i] = models.WaveSample{
			Voltage: volt[i+window],
			Current: curr[i+window],
		}
	}
	dp := NewDualPhasorPipeline(window, targetHz, sampleRate)
	vOut := make([]models.Phasor, batchSize)
	iOut := make([]models.Phasor, batchSize)
	for i := 0; i < window; i++ {
		dp.ProcessSample(models.WaveSample{Voltage: volt[i], Current: curr[i]})
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dp.ProcessBatch(samples, vOut, iOut)
	}
}

func BenchmarkGoertzel(b *testing.B) {
	N := 1024
	targetHz := 40000.0
	sampleRate := 100000.0
	sig := generateSineWave(N, targetHz, sampleRate, 512, 0)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		g := NewGoertzel(targetHz, sampleRate, N)
		g.AddBatch(sig)
		_ = g.Result()
	}
}

func BenchmarkSlidingDFT_Update(b *testing.B) {
	sig := generateSineWave(b.N+512, 40_000, 100_000, 511, 0.5)
	sd := NewSlidingDFT(512, 40_000, 100_000, WindowHann)
	for i := 0; i < 512; i++ {
		sd.Update(sig[i], uint64(i)*10_000)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sd.Update(sig[512+i], uint64(512+i)*10_000)
	}
}

func BenchmarkDualPhasor_Process(b *testing.B) {
	vs := generateSineWave(b.N+512, 40_000, 100_000, 511, 0)
	cs := generateSineWave(b.N+512, 40_000, 100_000, 255, 0.3)
	p := NewDualPhasorPipeline(512, 40_000, 100_000)
	for i := 0; i < 512; i++ {
		s := models.WaveSample{
			TimestampNs: uint64(i) * 10_000,
			SeqNo:       uint32(i),
			Voltage:     vs[i],
			Current:     cs[i],
		}
		p.ProcessSample(s)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := models.WaveSample{
			TimestampNs: uint64(512+i) * 10_000,
			SeqNo:       uint32(512 + i),
			Voltage:     vs[512+i],
			Current:     cs[512+i],
		}
		p.ProcessSample(s)
	}
}
