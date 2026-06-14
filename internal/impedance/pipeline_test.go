package impedance

import (
	"math"
	"testing"

	"wirebonding/ultra/pkg/models"
)

func generateWaveSamples(N int, targetHz float64, sampleRate float64,
	vAmp, iAmp float64, phaseDiff float64, zStepAt int) []models.WaveSample {
	out := make([]models.WaveSample, N)
	for i := 0; i < N; i++ {
		t := float64(i) / sampleRate
		vPhase := 2.0 * math.Pi * targetHz * t
		iPhase := vPhase - phaseDiff
		v := vAmp * math.Sin(vPhase)
		cur := iAmp * math.Sin(iPhase)
		if zStepAt > 0 && i >= zStepAt {
			cur *= 0.7
		}
		out[i] = models.WaveSample{
			TimestampNs: uint64(i) * 10000,
			Voltage:     v,
			Current:     cur,
			SeqNo:       uint32(i),
		}
	}
	return out
}

func TestPipeline_SolveImpedance(t *testing.T) {
	const (
		window     = 512
		targetHz   = 40000.0
		sampleRate = 100000.0
		N          = 8192
	)
	rOhm := 5.0
	xOhm := 3.0
	zMag := math.Sqrt(rOhm*rOhm + xOhm*xOhm)
	phaseDiff := math.Atan2(xOhm, rOhm)
	iAmp := 100.0
	vAmp := iAmp * zMag
	samples := generateWaveSamples(N, targetHz, sampleRate, vAmp, iAmp, phaseDiff, -1)
	cfg := DefaultConfig("CH01")
	cfg.WindowSize = window
	cfg.TargetHz = targetHz
	cfg.SampleHz = sampleRate
	cfg.MatrixRows = 4096
	p := NewPipeline(cfg)
	frames := make([]models.PhasorFrame, N)
	anoms := make([]AnomalyEvent, 128)
	fc, ac := p.ProcessBatch(samples, frames, anoms)
	if fc == 0 {
		t.Fatal("no frames ready")
	}
	_ = ac
	lastN := frames[fc-1]
	measuredZ := lastN.Impedance.Magnitude
	tol := zMag * 0.15
	if math.Abs(measuredZ-zMag) > tol {
		t.Errorf("impedance magnitude: got %.4fΩ expected≈%.4fΩ (tol=%.4fΩ)",
			measuredZ, zMag, tol)
	}
}

func TestPipeline_AnomalyDetection(t *testing.T) {
	const (
		window     = 512
		targetHz   = 40000.0
		sampleRate = 100000.0
		N          = 16384
		stepAt     = 6000
	)
	rOhm := 5.0
	xOhm := 3.0
	zMag := math.Sqrt(rOhm*rOhm + xOhm*xOhm)
	phaseDiff := math.Atan2(xOhm, rOhm)
	iAmp := 100.0
	vAmp := iAmp * zMag
	samples := generateWaveSamples(N, targetHz, sampleRate, vAmp, iAmp, phaseDiff, stepAt)
	cfg := DefaultConfig("CH01")
	cfg.WindowSize = window
	cfg.TargetHz = targetHz
	cfg.SampleHz = sampleRate
	cfg.MatrixRows = 8192
	p := NewPipeline(cfg)
	p.SetJumpThreshold(15.0)
	frames := make([]models.PhasorFrame, N)
	anoms := make([]AnomalyEvent, 256)
	fc, ac := p.ProcessBatch(samples, frames, anoms)
	if fc == 0 {
		t.Fatal("no frames ready")
	}
	if ac == 0 {
		t.Errorf("expected anomalies after impedance step at %d, got 0 (frames=%d)", stepAt, fc)
	}
}

func TestSpatiotemporalMatrix_BaselineLock(t *testing.T) {
	const (
		window     = 512
		targetHz   = 40000.0
		sampleRate = 100000.0
		N          = 2048
	)
	samples := generateWaveSamples(N, targetHz, sampleRate, 500, 100, 0.7, -1)
	cfg := DefaultConfig("CH02")
	cfg.WindowSize = window
	cfg.TargetHz = targetHz
	cfg.SampleHz = sampleRate
	cfg.MatrixRows = 1024
	p := NewPipeline(cfg)
	frames := make([]models.PhasorFrame, N)
	anoms := make([]AnomalyEvent, 16)
	p.ProcessBatch(samples, frames, anoms)
	if !p.BaselineLocked() {
		t.Errorf("baseline should be locked after %d processed samples", p.TotalReady())
	}
}

func TestSnapshot(t *testing.T) {
	const (
		window     = 512
		targetHz   = 40000.0
		sampleRate = 100000.0
		N          = 4096
	)
	samples := generateWaveSamples(N, targetHz, sampleRate, 500, 100, 0.7, -1)
	cfg := DefaultConfig("CH03")
	cfg.WindowSize = window
	cfg.TargetHz = targetHz
	cfg.SampleHz = sampleRate
	cfg.MatrixRows = 2048
	p := NewPipeline(cfg)
	frames := make([]models.PhasorFrame, N)
	anoms := make([]AnomalyEvent, 16)
	p.ProcessBatch(samples, frames, anoms)
	_, snap, _ := p.Snapshot(100)
	if snap.ChannelID != "CH03" {
		t.Errorf("snapshot channel: got %s expected CH03", snap.ChannelID)
	}
	if snap.Rows != 100 {
		t.Errorf("snapshot rows: got %d expected 100", snap.Rows)
	}
	if len(snap.ZMag) != 100 {
		t.Errorf("snapshot ZMag length: got %d expected 100", len(snap.ZMag))
	}
}

func BenchmarkPipelineProcess(b *testing.B) {
	const (
		window     = 512
		targetHz   = 40000.0
		sampleRate = 100000.0
		batchSize  = 8192
	)
	samples := generateWaveSamples(batchSize+window, targetHz, sampleRate, 500, 100, 0.7, -1)
	cfg := DefaultConfig("BENCH")
	cfg.WindowSize = window
	cfg.TargetHz = targetHz
	cfg.SampleHz = sampleRate
	cfg.MatrixRows = 16384
	p := NewPipeline(cfg)
	frames := make([]models.PhasorFrame, batchSize)
	anoms := make([]AnomalyEvent, 64)
	for i := 0; i < window; i++ {
		p.Process(samples[i])
	}
	workSet := samples[window : window+batchSize]
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.ProcessBatch(workSet, frames, anoms)
	}
}

func BenchmarkMatrixAppendRow(b *testing.B) {
	m := NewSpatiotemporalMatrix(2048, "BENCH")
	vPh := models.Phasor{Real: 300, Imag: 400, Magnitude: 500, Angle: 0.927}
	iPh := models.Phasor{Real: 50, Imag: 30, Magnitude: 58.3, Angle: 0.540}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.AppendRow(uint32(i), uint64(i)*10000, vPh, iPh)
	}
}
