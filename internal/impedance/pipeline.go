package impedance

import (
	"sync"
	"sync/atomic"
	"time"

	"wirebonding/ultra/internal/dsp"
	"wirebonding/ultra/internal/pullstrength"
	"wirebonding/ultra/pkg/models"
)

type Pipeline struct {
	mu          sync.Mutex
	phasor      *dsp.DualPhasorPipeline
	matrix      *SpatiotemporalMatrix
	meyer       *pullstrength.MeyerPredictor
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

	pullEvents   []pullstrength.PullEvent
	maxPull      int
	pullHead     int
	pullFull     bool
	pullEventSeq atomic.Uint64
	pullReady    atomic.Bool
}

type PipelineConfig struct {
	ChannelID       string
	WindowSize      int
	TargetHz        float64
	SampleHz        float64
	MatrixRows      int
	MaxAnomalies    int
	EnablePullPred  bool
	MaxPullEvents   int
	PullRedlineG    float64
}

func DefaultConfig(channelID string) PipelineConfig {
	return PipelineConfig{
		ChannelID:      channelID,
		WindowSize:     512,
		TargetHz:       40_000.0,
		SampleHz:       100_000.0,
		MatrixRows:     2048,
		MaxAnomalies:   1024,
		EnablePullPred: true,
		MaxPullEvents:  1024,
		PullRedlineG:   5.0,
	}
}

func NewPipeline(cfg PipelineConfig) *Pipeline {
	if cfg.WindowSize <= 0 {
		cfg.WindowSize = 512
	}
	if cfg.TargetHz <= 0 {
		cfg.TargetHz = 40_000.0
	}
	if cfg.SampleHz <= 0 {
		cfg.SampleHz = 100_000.0
	}
	if cfg.MaxAnomalies <= 0 {
		cfg.MaxAnomalies = 1024
	}
	if cfg.MaxPullEvents <= 0 {
		cfg.MaxPullEvents = 1024
	}
	if cfg.PullRedlineG <= 0 {
		cfg.PullRedlineG = 5.0
	}
	p := &Pipeline{
		phasor:     dsp.NewDualPhasorPipeline(cfg.WindowSize, cfg.TargetHz, cfg.SampleHz),
		matrix:     NewSpatiotemporalMatrix(cfg.MatrixRows, cfg.ChannelID),
		channelID:  cfg.ChannelID,
		windowSize: cfg.WindowSize,
		targetHz:   cfg.TargetHz,
		sampleHz:   cfg.SampleHz,
		anomalies:  make([]AnomalyEvent, cfg.MaxAnomalies),
		maxAnom:    cfg.MaxAnomalies,
		pullEvents: make([]pullstrength.PullEvent, cfg.MaxPullEvents),
		maxPull:    cfg.MaxPullEvents,
	}
	if cfg.EnablePullPred {
		mcfg := pullstrength.DefaultMeyerConfig()
		mcfg.RedlineGrams = cfg.PullRedlineG
		mcfg.BondingFreqHz = cfg.TargetHz
		p.meyer = pullstrength.NewMeyerPredictor(mcfg)
	}
	return p
}

func (p *Pipeline) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phasor.Reset()
	p.matrix.Reset()
	if p.meyer != nil {
		p.meyer.Reset()
	}
	p.initSamples = 0
	p.started = false
	p.totalIn = 0
	p.totalReady = 0
	for i := range p.anomalies {
		p.anomalies[i] = AnomalyEvent{}
	}
	p.anomHead = 0
	p.anomFull = false
	for i := range p.pullEvents {
		p.pullEvents[i] = pullstrength.PullEvent{}
	}
	p.pullHead = 0
	p.pullFull = false
	p.pullReady.Store(false)
}

func (p *Pipeline) Process(sample models.WaveSample) (readyFrame *models.PhasorFrame, anomaly *AnomalyEvent, pullEvt *pullstrength.PullEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.totalIn++
	p.initSamples++

	vPh, iPh, phReady := p.phasor.ProcessSample(sample)
	if !phReady {
		return nil, nil, nil
	}
	p.totalReady++
	p.started = true

	zp, anom, ok := p.matrix.AppendRow(sample.SeqNo, sample.TimestampNs, *vPh, *iPh)
	if !ok {
		return nil, nil, nil
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

	if p.meyer != nil {
		pred, predReady := p.meyer.PushPhasor(sample.SeqNo, sample.TimestampNs, *vPh, *iPh, zp)
		if predReady && pred.BelowRedline {
			p.pullReady.Store(true)
			eid := p.pullEventSeq.Add(1)
			evt := pullstrength.PullEvent{
				ChannelID:   p.channelID,
				Prediction:  *pred,
				EventID:     eid,
				TriggeredAt: time.Now(),
			}
			p.pullEvents[p.pullHead] = evt
			p.pullHead = (p.pullHead + 1) % p.maxPull
			if p.pullHead == 0 {
				p.pullFull = true
			}
			pullEvt = &evt
		}
	}

	outFrame := p.latestFrame
	return &outFrame, anom, pullEvt
}

func (p *Pipeline) ProcessBatch(samples []models.WaveSample,
	frameOut []models.PhasorFrame, anomOut []AnomalyEvent, pullOut []pullstrength.PullEvent) (frameCount, anomCount, pullCount int) {
	fc, ac, pc := 0, 0, 0
	for _, s := range samples {
		fr, anom, pullEvt := p.Process(s)
		if fr != nil && fc < len(frameOut) {
			frameOut[fc] = *fr
			fc++
		}
		if anom != nil && ac < len(anomOut) {
			anomOut[ac] = *anom
			ac++
		}
		if pullEvt != nil && pc < len(pullOut) {
			pullOut[pc] = *pullEvt
			pc++
		}
	}
	return fc, ac, pc
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

func (p *Pipeline) PullPrediction() (pullstrength.PullStrengthPrediction, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meyer == nil {
		return pullstrength.PullStrengthPrediction{}, false
	}
	return p.meyer.Latest()
}

func (p *Pipeline) PullTotal() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meyer == nil {
		return 0
	}
	return p.meyer.TotalPredictions()
}

func (p *Pipeline) PullRedlineHits() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meyer == nil {
		return 0
	}
	return p.meyer.RedlineHits()
}

func (p *Pipeline) PullRedlineGrams() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meyer == nil {
		return 0
	}
	return p.meyer.RedlineGrams()
}

func (p *Pipeline) SetPullRedline(grams float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meyer != nil {
		p.meyer.SetRedline(grams)
	}
}

func (p *Pipeline) RecentPullEvents(count int) []pullstrength.PullEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := p.maxPull
	if !p.pullFull {
		total = p.pullHead
	}
	if count <= 0 || count > total {
		count = total
	}
	out := make([]pullstrength.PullEvent, count)
	end := p.pullHead
	start := end - count
	if start < 0 {
		start += p.maxPull
	}
	for i := 0; i < count; i++ {
		out[i] = p.pullEvents[(start+i)%p.maxPull]
	}
	return out
}
