package pullstrength

import (
	"math"
	"testing"

	"wirebonding/ultra/pkg/models"
)

func TestMeyerPredictor_SofteningExtraction(t *testing.T) {
	cfg := DefaultMeyerConfig()
	cfg.RedlineGrams = 10.0
	mp := NewMeyerPredictor(cfg)

	const N = 256
	var okCount int
	for i := 0; i < N; i++ {
		amp := 1.0 + 0.5*math.Sin(float64(i)*0.1)
		tsNs := uint64(i) * 10_000
		vPh := mkPhasor(amp*1.0, amp*0.2, 40_000, 0.3)
		iPh := mkPhasor(amp*0.1, amp*0.02, 40_000, 0.0)
		zp := mkImpedance(tsNs, 12.0+2.0*math.Sin(float64(i)*0.05), 5.0, uint32(i))

		pred, ok := mp.PushPhasor(uint32(i), tsNs, vPh, iPh, zp)
		if ok {
			okCount++
			if pred.PullGrams <= 0 {
				t.Fatalf("non-positive pull: %f", pred.PullGrams)
			}
		}
	}
	if okCount == 0 {
		t.Fatal("no predictions generated")
	}
	if mp.TotalPredictions() == 0 {
		t.Fatal("predictions counter zero")
	}
	t.Logf("predictions ok=%d of %d total=%d", okCount, N, mp.TotalPredictions())
}

func TestMeyerPredictor_Redline5Grams(t *testing.T) {
	cfg := DefaultMeyerConfig()
	cfg.RedlineGrams = 5.0
	mp := NewMeyerPredictor(cfg)

	for i := 0; i < 128; i++ {
		tsNs := uint64(i) * 10_000
		vPh := mkPhasor(0.5, 0.0, 40_000, 0.8)
		iPh := mkPhasor(0.8, 0.0, 40_000, 0.0)
		zp := mkImpedance(tsNs, 1.5, 0.5, uint32(i))
		_, _ = mp.PushPhasor(uint32(i), tsNs, vPh, iPh, zp)
	}

	latest, ok := mp.Latest()
	if !ok {
		t.Fatal("no latest prediction")
	}
	t.Logf("latest: pull=%.3fg shear=%.3fg conf=%.2f below5g=%v hits=%d",
		latest.PullGrams, latest.ShearGrams, latest.Confidence,
		latest.BelowRedline, mp.RedlineHits())
}

func TestMeyerPredictor_SetRedline(t *testing.T) {
	cfg := DefaultMeyerConfig()
	mp := NewMeyerPredictor(cfg)
	if mp.RedlineGrams() != 5.0 {
		t.Errorf("default redline = %f want 5.0", mp.RedlineGrams())
	}
	mp.SetRedline(10.0)
	if mp.RedlineGrams() != 10.0 {
		t.Errorf("set redline = %f want 10.0", mp.RedlineGrams())
	}
}

func TestMeyerPredictor_Reset(t *testing.T) {
	cfg := DefaultMeyerConfig()
	mp := NewMeyerPredictor(cfg)
	for i := 0; i < 128; i++ {
		tsNs := uint64(i) * 10_000
		vPh := mkPhasor(1.0, 0.0, 40_000, 0.0)
		iPh := mkPhasor(0.1, 0.0, 40_000, 0.0)
		zp := mkImpedance(tsNs, 10.0, 3.0, uint32(i))
		_, _ = mp.PushPhasor(uint32(i), tsNs, vPh, iPh, zp)
	}
	if mp.TotalPredictions() == 0 {
		t.Fatal("expected predictions before reset")
	}
	mp.Reset()
	if mp.TotalPredictions() != 0 {
		t.Errorf("after reset total=%d want 0", mp.TotalPredictions())
	}
	if mp.RedlineHits() != 0 {
		t.Errorf("after reset hits=%d want 0", mp.RedlineHits())
	}
}

func TestBondingFeatureVector_EnergyRatio(t *testing.T) {
	cfg := DefaultMeyerConfig()
	cfg.RedlineGrams = 100.0
	mp := NewMeyerPredictor(cfg)

	const N = 1024
	lastPred := PullStrengthPrediction{}
	for i := 0; i < N; i++ {
		amp := 0.5 + float64(i)/float64(N)
		tsNs := uint64(i) * 10_000
		vPh := mkPhasor(amp, amp*0.1, 40_000, 0.5)
		iPh := mkPhasor(amp*0.2, amp*0.02, 40_000, 0.0)
		zp := mkImpedance(tsNs, 5.0, 2.0, uint32(i))
		p, ok := mp.PushPhasor(uint32(i), tsNs, vPh, iPh, zp)
		if ok {
			lastPred = *p
		}
	}
	if lastPred.Features.DissipationJ <= 0 {
		t.Errorf("expected positive dissipation, got %f", lastPred.Features.DissipationJ)
	}
	t.Logf("dissipation=%.6fJ energy_ratio=%.3f pull=%.2fg conf=%.2f",
		lastPred.Features.DissipationJ, lastPred.Features.EnergyRatio,
		lastPred.PullGrams, lastPred.Confidence)
}

func BenchmarkMeyerPredictor_Push(b *testing.B) {
	cfg := DefaultMeyerConfig()
	mp := NewMeyerPredictor(cfg)
	ts := uint64(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts += 10_000
		vPh := mkPhasor(1.0, 0.3, 40_000, 0.3)
		iPh := mkPhasor(0.2, 0.05, 40_000, 0.0)
		zp := mkImpedance(ts, 10.0, 4.0, uint32(i))
		_, _ = mp.PushPhasor(uint32(i), ts, vPh, iPh, zp)
	}
}

func mkPhasor(re, im, freq, ang float64) models.Phasor {
	return models.Phasor{
		Real:      re,
		Imag:      im,
		Magnitude: math.Hypot(re, im),
		Angle:     ang,
		FreqHz:    freq,
	}
}

func mkImpedance(tsNs uint64, r, x float64, seq uint32) models.ImpedancePoint {
	return models.ImpedancePoint{
		TimestampNs: tsNs,
		Resistance:  r,
		Reactance:   x,
		Magnitude:   math.Hypot(r, x),
		Phase:       math.Atan2(x, r),
		SeqNo:       seq,
	}
}
