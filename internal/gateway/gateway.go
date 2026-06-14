package gateway

import (
	"context"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"wirebonding/ultra/internal/iec61850"
	"wirebonding/ultra/internal/impedance"
	"wirebonding/ultra/internal/packetcap"
	"wirebonding/ultra/internal/ringbuf"
	"wirebonding/ultra/pkg/models"
)

type Config struct {
	InterfaceName string
	ChannelID     string
	ZeroCopy      bool
	Promisc       bool
	EtherType     uint16
	TargetHz      float64
	WindowSize    int
	MatrixRows    int
	Simulate      bool
	BatchSize     int
	StatsInterval time.Duration
	LogPath       string
	RingCap       int
	WorkerCount   int
}

func DefaultConfig() Config {
	return Config{
		InterfaceName: "eth0",
		ChannelID:     "WBBOND-CH01",
		ZeroCopy:      true,
		Promisc:       true,
		EtherType:     iec61850.EtherTypeSV,
		TargetHz:      40_000.0,
		WindowSize:    512,
		MatrixRows:    2048,
		Simulate:      true,
		BatchSize:     256,
		StatsInterval: 5 * time.Second,
		RingCap:       65536,
		WorkerCount:   64,
	}
}

type Stats struct {
	FramesRx       uint64
	FramesDropped  uint64
	BytesRx        uint64
	SamplesRx      uint64
	FramesReady    uint64
	Anomalies      uint64
	ParseErrors    uint64
	AvgLatencyNs   uint64
	MaxLatencyNs   uint64
	UptimeNs       uint64
	RingEnqFails   uint64
	RingDeqFails   uint64
}

type frameMsg struct {
	frame *models.RawFrame
	rxTs  uint64
}

type sampleMsg struct {
	samples []models.WaveSample
	rxTs    uint64
}

type WireBondingGateway struct {
	mu        sync.Mutex
	cfg       Config
	capture   *packetcap.PacketCapture
	decoder   *iec61850.Decoder
	pipeline  *impedance.Pipeline

	frameRing *ringbuf.BatchRingBuffer[frameMsg]
	sampRing  *ringbuf.BatchRingBuffer[sampleMsg]

	rawBatch  []*models.RawFrame
	parseBuf  []models.WaveSample
	frameBuf  []models.PhasorFrame
	anomBuf   []impedance.AnomalyEvent

	stats     Stats
	startTime time.Time
	running   atomic.Bool
	logger    *log.Logger
	logFile   *os.File

	anomalyCB func(impedance.AnomalyEvent)
	frameCB   func(models.PhasorFrame)
}

func NewWireBondingGateway(cfg Config) (*WireBondingGateway, error) {
	if cfg.ChannelID == "" {
		cfg.ChannelID = "WBBOND-CH01"
	}
	if cfg.WindowSize <= 0 {
		cfg.WindowSize = 512
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 256
	}
	if cfg.TargetHz <= 0 {
		cfg.TargetHz = 60_000.0
	}
	if cfg.RingCap <= 0 {
		cfg.RingCap = 65536
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 64
	}

	cap, err := packetcap.NewPacketCapture(cfg.InterfaceName, cfg.EtherType, cfg.Promisc, cfg.ZeroCopy)
	if err != nil {
		return nil, err
	}

	dec := iec61850.NewDecoder()
	pipeCfg := impedance.DefaultConfig(cfg.ChannelID)
	pipeCfg.WindowSize = cfg.WindowSize
	pipeCfg.TargetHz = cfg.TargetHz
	pipeCfg.SampleHz = float64(models.SampleRateHz)
	pipeCfg.MatrixRows = cfg.MatrixRows
	pipe := impedance.NewPipeline(pipeCfg)

	gw := &WireBondingGateway{
		cfg:       cfg,
		capture:   cap,
		decoder:   dec,
		pipeline:  pipe,
		frameRing: ringbuf.NewBatch[frameMsg](cfg.RingCap),
		sampRing:  ringbuf.NewBatch[sampleMsg](cfg.RingCap),
		rawBatch:  make([]*models.RawFrame, cfg.BatchSize),
		parseBuf:  make([]models.WaveSample, cfg.BatchSize*128),
		frameBuf:  make([]models.PhasorFrame, cfg.BatchSize*128),
		anomBuf:   make([]impedance.AnomalyEvent, 128),
	}

	if cfg.LogPath != "" {
		f, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			gw.logFile = f
			gw.logger = log.New(f, "[WBGW] ", log.LstdFlags|log.Lmicroseconds)
		}
	}
	if gw.logger == nil {
		gw.logger = log.New(os.Stdout, "[WBGW] ", log.LstdFlags|log.Lmicroseconds)
	}
	return gw, nil
}

func (gw *WireBondingGateway) OnAnomaly(cb func(impedance.AnomalyEvent)) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.anomalyCB = cb
}

func (gw *WireBondingGateway) OnFrame(cb func(models.PhasorFrame)) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.frameCB = cb
}

func (gw *WireBondingGateway) Start(ctx context.Context) error {
	if gw.running.Load() {
		return nil
	}
	gw.running.Store(true)
	gw.startTime = time.Now()

	gw.logger.Printf("Starting gateway: iface=%s channel=%s simulate=%v target=%.1fHz window=%d ring=%d workers=%d",
		gw.cfg.InterfaceName, gw.cfg.ChannelID, gw.cfg.Simulate, gw.cfg.TargetHz, gw.cfg.WindowSize,
		gw.cfg.RingCap, gw.cfg.WorkerCount)

	errChan := make(chan error, 4)
	go gw.captureLoop(ctx, errChan)
	go gw.decodeLoop(ctx, errChan)
	go gw.dspLoop(ctx, errChan)
	go gw.statsReporter(ctx)

	var firstErr error
	select {
	case <-ctx.Done():
	case firstErr = <-errChan:
	}
	gw.Stop()
	return firstErr
}

func (gw *WireBondingGateway) captureLoop(ctx context.Context, errChan chan<- error) {
	for gw.running.Load() && ctx.Err() == nil {
		var n int
		if gw.cfg.Simulate {
			n = gw.capture.SimulateBatch(gw.rawBatch, 80, uint32(atomic.LoadUint64(&gw.stats.SamplesRx)))
		} else {
			n = gw.capture.ReadBatch(gw.rawBatch)
		}
		if n <= 0 {
			continue
		}

		atomic.AddUint64(&gw.stats.FramesRx, uint64(n))

		for i := 0; i < n; i++ {
			frame := gw.rawBatch[i]
			if frame == nil {
				continue
			}
			atomic.AddUint64(&gw.stats.BytesRx, uint64(frame.Length))
			msg := frameMsg{frame: frame, rxTs: uint64(time.Now().UnixNano())}
			if !gw.frameRing.TryEnqueue(msg) {
				atomic.AddUint64(&gw.stats.RingEnqFails, 1)
				atomic.AddUint64(&gw.stats.FramesDropped, 1)
			}
		}
	}
}

func (gw *WireBondingGateway) decodeLoop(ctx context.Context, errChan chan<- error) {
	batchBuf := make([]frameMsg, 64)
	for gw.running.Load() && ctx.Err() == nil {
		n := gw.frameRing.DequeueBatch(batchBuf)
		if n == 0 {
			runtimePause()
			continue
		}

		totalSamples := 0
		for i := 0; i < n; i++ {
			msg := batchBuf[i]
			if msg.frame == nil {
				continue
			}
			_, samples, err := gw.decoder.DecodeFull(msg.frame)
			if err != nil {
				atomic.AddUint64(&gw.stats.ParseErrors, 1)
				continue
			}
			if len(samples) > 0 {
				sMsg := sampleMsg{samples: make([]models.WaveSample, len(samples)), rxTs: msg.rxTs}
				copy(sMsg.samples, samples)
				if !gw.sampRing.TryEnqueue(sMsg) {
					atomic.AddUint64(&gw.stats.RingEnqFails, 1)
				}
				totalSamples += len(samples)
			}
		}
		if totalSamples > 0 {
			atomic.AddUint64(&gw.stats.SamplesRx, uint64(totalSamples))
		}
	}
}

func (gw *WireBondingGateway) dspLoop(ctx context.Context, errChan chan<- error) {
	sampBuf := make([]sampleMsg, 64)
	workBuf := make([]models.WaveSample, 0, 8192)
	for gw.running.Load() && ctx.Err() == nil {
		n := gw.sampRing.DequeueBatch(sampBuf)
		if n == 0 {
			runtimePause()
			continue
		}

		workBuf = workBuf[:0]
		for i := 0; i < n; i++ {
			workBuf = append(workBuf, sampBuf[i].samples...)
		}
		if len(workBuf) == 0 {
			continue
		}

		dsStart := time.Now()
		fc, ac := gw.pipeline.ProcessBatch(workBuf, gw.frameBuf, gw.anomBuf)
		dsLatency := time.Since(dsStart)

		if fc > 0 {
			atomic.AddUint64(&gw.stats.FramesReady, uint64(fc))
			if gw.frameCB != nil {
				for i := 0; i < fc; i++ {
					gw.frameCB(gw.frameBuf[i])
				}
			}
		}
		if ac > 0 {
			atomic.AddUint64(&gw.stats.Anomalies, uint64(ac))
			if gw.anomalyCB != nil {
				for i := 0; i < ac; i++ {
					gw.anomalyCB(gw.anomBuf[i])
					gw.logger.Printf("ANOMALY id=%d seq=%d dev=%.2f%% phase=%.2fdeg",
						gw.anomBuf[i].AlarmID, gw.anomBuf[i].SeqNo,
						gw.anomBuf[i].DeviationPct, gw.anomBuf[i].PhaseShiftDeg)
				}
			}
		}

		totalLatencyNs := uint64(dsLatency.Nanoseconds())
		atomic.AddUint64(&gw.stats.AvgLatencyNs, totalLatencyNs/uint64(maxInt(len(workBuf), 1)))
		prevMax := atomic.LoadUint64(&gw.stats.MaxLatencyNs)
		if totalLatencyNs > prevMax {
			atomic.CompareAndSwapUint64(&gw.stats.MaxLatencyNs, prevMax, totalLatencyNs)
		}
	}
}

func runtimePause() {
	time.Sleep(time.Microsecond * 10)
}

func (gw *WireBondingGateway) statsReporter(ctx context.Context) {
	if gw.cfg.StatsInterval <= 0 {
		return
	}
	ticker := time.NewTicker(gw.cfg.StatsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !gw.running.Load() {
				return
			}
			gw.logStats()
		}
	}
}

func (gw *WireBondingGateway) logStats() {
	s := gw.GetStats()
	uptime := time.Duration(s.UptimeNs)
	capStats := gw.capture.Stats()
	fps := float64(s.FramesRx) / maxFloat(uptime.Seconds(), 1e-9)
	sps := float64(s.SamplesRx) / maxFloat(uptime.Seconds(), 1e-9)
	gw.logger.Printf(
		"STATS uptime=%s frames_rx=%d (%.1f/s) samples=%d (%.1f/s) ready=%d anom=%d parse_err=%d "+
			"avg_lat=%.3fus max_lat=%.3fus cap_drop=%d ring_enq_fail=%d ring_deq_fail=%d",
		uptime.Round(100*time.Millisecond),
		s.FramesRx, fps,
		s.SamplesRx, sps,
		s.FramesReady, s.Anomalies, s.ParseErrors,
		float64(s.AvgLatencyNs)/1e3,
		float64(s.MaxLatencyNs)/1e3,
		capStats.DropPackets,
		s.RingEnqFails, s.RingDeqFails,
	)
}

func (gw *WireBondingGateway) GetStats() Stats {
	uptime := time.Since(gw.startTime).Nanoseconds()
	framesReady := atomic.LoadUint64(&gw.stats.FramesReady)
	anomalies := atomic.LoadUint64(&gw.stats.Anomalies)
	avgLatency := uint64(0)
	if framesReady > 0 {
		avgLatency = atomic.LoadUint64(&gw.stats.AvgLatencyNs) / framesReady
	}
	return Stats{
		FramesRx:      atomic.LoadUint64(&gw.stats.FramesRx),
		FramesDropped: atomic.LoadUint64(&gw.stats.FramesDropped),
		BytesRx:       atomic.LoadUint64(&gw.stats.BytesRx),
		SamplesRx:     atomic.LoadUint64(&gw.stats.SamplesRx),
		FramesReady:   framesReady,
		Anomalies:     anomalies,
		ParseErrors:   atomic.LoadUint64(&gw.stats.ParseErrors),
		AvgLatencyNs:  avgLatency,
		MaxLatencyNs:  atomic.LoadUint64(&gw.stats.MaxLatencyNs),
		UptimeNs:      uint64(uptime),
		RingEnqFails:  atomic.LoadUint64(&gw.stats.RingEnqFails),
		RingDeqFails:  atomic.LoadUint64(&gw.stats.RingDeqFails),
	}
}

func (gw *WireBondingGateway) Stop() {
	if !gw.running.CompareAndSwap(true, false) {
		return
	}
	gw.logStats()
	if gw.capture != nil {
		gw.capture.Close()
	}
	if gw.logFile != nil {
		gw.logFile.Close()
	}
}

func (gw *WireBondingGateway) Snapshot(lastN int) (models.PhasorFrame, impedance.MatrixSnapshot, uint64) {
	return gw.pipeline.Snapshot(lastN)
}

func (gw *WireBondingGateway) RecentAnomalies(count int) []impedance.AnomalyEvent {
	return gw.pipeline.RecentAnomalies(count)
}

func (gw *WireBondingGateway) Pipeline() *impedance.Pipeline { return gw.pipeline }
func (gw *WireBondingGateway) SetJumpThreshold(pct float64)  { gw.pipeline.SetJumpThreshold(pct) }
func (gw *WireBondingGateway) SetPhaseThreshold(deg float64) { gw.pipeline.SetPhaseThreshold(deg) }
func (gw *WireBondingGateway) FrameRing() *ringbuf.BatchRingBuffer[frameMsg] { return gw.frameRing }
func (gw *WireBondingGateway) SampleRing() *ringbuf.BatchRingBuffer[sampleMsg] { return gw.sampRing }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
