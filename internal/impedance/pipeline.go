package impedance

import (
	"sync"

	"wirebonding/ultra/internal/dsp"
	"wirebonding/ultra/pkg/models"
)

type Pipeline struct {
	mu          sync.Mutex
	phasor      *dsp.DualPhasorPipeline
	matrix      *SpatiotemporalMatrix
	channelID   string
	windowSize  int
	targetHz    float64
	sampleHz    float64
	started     bool
	initSamples int
	totalIn     uint64
	totalReady  uint64

	latestFrame models.PhasorFrame
	anomalies   []AnomalyEvent
	maxAnom     int
	anomHead    int
	anomFull    bool
}

type PipelineConfig struct {
	ChannelID    string
	WindowSize   int
	TargetHz     float64
	SampleHz     float64
	MatrixRows   int
	MaxAnomalies int
}

func DefaultConfig(channelID string) PipelineConfig {
	return PipelineConfig{
		ChannelID:    channelID,
		WindowSize:   512,
		TargetHz:     40_000.0,
		SampleHz:     100_000.0,
		MatrixRows:   2048,
		MaxAnomalies: 1024,
	}
}

func NewPipeline(cfg PipelineConfig) *Pipeline {
	if cfg.WindowSize <= 0 {
		cfg.WindowSize = 512
	}
	if cfg.TargetHz <= 0 {
		cfg.TargetHz = 60_000.0
	}
	if cfg.SampleHz <= 0 {
		cfg.SampleHz = float64(models.SampleRateHz)
	}
	if cfg.MaxAnomalies <= 0 {
		cfg.MaxAnomalies = 1024
	}
	return &Pipeline{
		phasor:     dsp.NewDualPhasorPipeline(cfg.WindowSize, cfg.TargetHz, cfg.SampleHz),
		matrix:     NewSpatiotemporalMatrix(cfg.MatrixRows, cfg.ChannelID),
		channelID:  cfg.ChannelID,
		windowSize: cfg.WindowSize,
		targetHz:   cfg.TargetHz,
		sampleHz:   cfg.SampleHz,
		anomalies:  make([]AnomalyEvent, cfg.MaxAnomalies),
		maxAnom:    cfg.MaxAnomalies,
	}
}

func (p *Pipeline) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phasor.Reset()
	p.matrix.Reset()
	p.initSamples = 0
	p.started = false
	p.totalIn = 0
	p.totalReady = 0
	for i := range p.anomalies {
		p.anomalies[i] = AnomalyEvent{}
	}
	p.anomHead = 0
	p.anomFull = false
}

func (p *Pipeline) Process(sample models.WaveSample) (readyFrame *models.PhasorFrame, anomaly *AnomalyEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.totalIn++
	p.initSamples++

	vPh, iPh, phReady := p.phasor.ProcessSample(sample)
	if !phReady {
		return nil, nil
	}
	p.totalReady++
	p.started = true

	zp, anom, ok := p.matrix.AppendRow(sample.SeqNo, sample.TimestampNs, *vPh, *iPh)
	if !ok {
		return nil, nil
	}
	p.latestFrame = models.PhasorFrame{
		SeqNo:         sample.SeqNo,
		TimestampNs:   sample.TimestampNs,
		VoltagePhasor: *vPh,
		CurrentPhasor: *iPh,
		Impedance:     zp,
	}
	if anom != nil {
		p.anomalies[p.anomHead] = *anom
		p.anomHead = (p.anomHead + 1) % p.maxAnom
		if p.anomHead == 0 {
			p.anomFull = true
		}
	}
	outFrame := p.latestFrame
	return &outFrame, anom
}

func (p *Pipeline) ProcessBatch(samples []models.WaveSample,
	frameOut []models.PhasorFrame, anomOut []AnomalyEvent) (frameCount, anomCount int) {
	fc, ac := 0, 0
	for _, s := range samples {
		fr, anom := p.Process(s)
		if fr != nil && fc < len(frameOut) {
			frameOut[fc] = *fr
			fc++
		}
		if anom != nil && ac < len(anomOut) {
			anomOut[ac] = *anom
			ac++
		}
	}
	return fc, ac
}

func (p *Pipeline) Latest() (models.PhasorFrame, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latestFrame, p.started
}

func (p *Pipeline) Snapshot(lastN int) (models.PhasorFrame, MatrixSnapshot, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := p.matrix.SnapshotRecent(lastN)
	return p.latestFrame, snap, p.matrix.AlarmCount()
}

func (p *Pipeline) RecentAnomalies(count int) []AnomalyEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := p.maxAnom
	if !p.anomFull {
		total = p.anomHead
	}
	if count <= 0 || count > total {
		count = total
	}
	out := make([]AnomalyEvent, count)
	end := p.anomHead
	start := end - count
	if start < 0 {
		start += p.maxAnom
	}
	for i := 0; i < count; i++ {
		out[i] = p.anomalies[(start+i)%p.maxAnom]
	}
	return out
}

func (p *Pipeline) SetJumpThreshold(pct float64)  { p.matrix.SetJumpThreshold(pct) }
func (p *Pipeline) SetPhaseThreshold(deg float64) { p.matrix.SetPhaseThreshold(deg) }
func (p *Pipeline) ChannelID() string             { return p.channelID }
func (p *Pipeline) BaselineLocked() bool          { return p.matrix.BaselineLocked() }
func (p *Pipeline) TotalIn() uint64               { return p.totalIn }
func (p *Pipeline) TotalReady() uint64            { return p.totalReady }
