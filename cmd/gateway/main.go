package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"wirebonding/ultra/internal/gateway"
	"wirebonding/ultra/internal/impedance"
)

func main() {
	var (
		iface     = flag.String("iface", "eth0", "network interface to monitor")
		channel   = flag.String("channel", "WBBOND-CH01", "bonding channel identifier")
		zerocopy  = flag.Bool("zcopy", true, "use AF_PACKET zero-copy (TPACKET_V3) on Linux")
		promisc   = flag.Bool("promisc", true, "enable promiscuous mode")
		simulate  = flag.Bool("sim", true, "run in simulation mode (realistic waveform injection)")
		hz        = flag.Float64("hz", 40000.0, "ultrasonic transducer fundamental frequency (Hz)")
		window    = flag.Int("window", 512, "DFT sliding window size (samples)")
		rows      = flag.Int("rows", 2048, "spatiotemporal matrix rows (ring size)")
		batch     = flag.Int("batch", 256, "packet batch read size")
		duration  = flag.Duration("duration", 0, "run duration (0 = until signal)")
		threshold = flag.Float64("threshold", 15.0, "impedance jump anomaly threshold (% deviation)")
		phaseDeg  = flag.Float64("phase", 12.0, "phase shift anomaly threshold (degrees)")
		statsIval = flag.Duration("stats", 5*time.Second, "stats reporting interval")
		logPath   = flag.String("log", "", "gateway log file path (default stdout)")
		snapshot  = flag.Int("snapshot", 0, "print last N impedance rows on shutdown")
		anomDump  = flag.Int("anom", 0, "print last N anomalies on shutdown")
	)
	flag.Parse()

	cfg := gateway.DefaultConfig()
	cfg.InterfaceName = *iface
	cfg.ChannelID = *channel
	cfg.ZeroCopy = *zerocopy
	cfg.Promisc = *promisc
	cfg.Simulate = *simulate
	cfg.TargetHz = *hz
	cfg.WindowSize = *window
	cfg.MatrixRows = *rows
	cfg.BatchSize = *batch
	cfg.StatsInterval = *statsIval
	cfg.LogPath = *logPath

	ctx, cancel := context.WithCancel(context.Background())
	if *duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case s := <-sigCh:
			fmt.Printf("\n[gateway] received signal %v, shutting down...\n", s)
			cancel()
		case <-ctx.Done():
		}
	}()

	gw, err := gateway.NewWireBondingGateway(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: failed to create gateway: %v\n", err)
		os.Exit(1)
	}
	defer gw.Stop()

	gw.SetJumpThreshold(*threshold)
	gw.SetPhaseThreshold(*phaseDeg)

	anomCount := uint64(0)
	gw.OnAnomaly(func(a impedance.AnomalyEvent) {
		anomCount++
	})

	fmt.Println("=== Wire Bonding Ultra - Realtime Impedance Gateway ===")
	fmt.Printf("  Interface : %s\n", *iface)
	fmt.Printf("  Channel   : %s\n", *channel)
	fmt.Printf("  Mode      : simulate=%v zcopy=%v promisc=%v\n", *simulate, *zerocopy, *promisc)
	fmt.Printf("  Target    : %.1f Hz  Window : %d  Matrix: %d\n", *hz, *window, *rows)
	fmt.Printf("  Thresholds: %.2f%% jump / %.2f° phase\n", *threshold, *phaseDeg)
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop.")
	fmt.Println()

	startTime := time.Now()
	err = gw.Start(ctx)
	elapsed := time.Since(startTime)

	fmt.Println()
	fmt.Println("=== Shutdown ===")
	s := gw.GetStats()
	fps := float64(s.FramesRx) / elapsed.Seconds()
	sps := float64(s.SamplesRx) / elapsed.Seconds()
	fmt.Printf("  Elapsed       : %s\n", elapsed.Round(100*time.Millisecond))
	fmt.Printf("  Frames RX     : %d (%.1f/s)\n", s.FramesRx, fps)
	fmt.Printf("  Samples RX    : %d (%.1f/s)\n", s.SamplesRx, sps)
	fmt.Printf("  Bytes RX      : %.2f MB\n", float64(s.BytesRx)/1048576.0)
	fmt.Printf("  Phasor Ready  : %d\n", s.FramesReady)
	fmt.Printf("  Parse Errors  : %d\n", s.ParseErrors)
	fmt.Printf("  Anomalies     : %d\n", anomCount)
	fmt.Printf("  Avg Latency   : %.3f us\n", float64(s.AvgLatencyNs)/1000.0)
	fmt.Printf("  Max Latency   : %.3f us\n", float64(s.MaxLatencyNs)/1000.0)

	if *snapshot > 0 {
		fmt.Println()
		fmt.Printf("=== Impedance Snapshot (last %d) ===\n", *snapshot)
		_, snap, _ := gw.Snapshot(*snapshot)
		fmt.Printf("  Channel       : %s\n", snap.ChannelID)
		fmt.Printf("  BaselineLocked: %v\n", snap.BaselineLocked)
		if snap.BaselineLocked {
			zr := real(snap.BaselineZ)
			zi := imag(snap.BaselineZ)
			zm := lenComplex(snap.BaselineZ)
			fmt.Printf("  Baseline Z    : R=%.3fΩ X=%.3fΩ |Z|=%.3fΩ\n", zr, zi, zm)
		}
		if snap.Rows > 0 {
			n := snap.Rows
			lastZ := snap.ZMag[n-1]
			lastPhase := snap.ZPhase[n-1] * 180.0 / 3.1415926535
			fmt.Printf("  Latest |Z|    : %.3f Ω  phase: %.2f°\n", lastZ, lastPhase)
			fmt.Printf("  Timestamp     : %d ns\n", snap.TimestampsNs[n-1])
			if n >= 10 {
				fmt.Println("  [last 10 |Z| values]:")
				for i := n - 10; i < n; i++ {
					fmt.Printf("    idx=%-5d  |Z|=%.4f Ω   ∠%.3f°   R=%.4fΩ X=%.4fΩ\n",
						i, snap.ZMag[i], snap.ZPhase[i]*180.0/3.1415926535,
						snap.ZReal[i], snap.ZImag[i])
				}
			}
		}
	}

	if *anomDump > 0 {
		anoms := gw.RecentAnomalies(*anomDump)
		if len(anoms) > 0 {
			fmt.Println()
			fmt.Printf("=== Recent Anomalies (last %d) ===\n", len(anoms))
			for i, a := range anoms {
				if a.AlarmID == 0 {
					continue
				}
				fmt.Printf("  #%d  id=%d seq=%d | dev=%.2f%%  phase=%.2f°  |Z0|=%.3fΩ |Zm|=%.3fΩ\n",
					i+1, a.AlarmID, a.SeqNo, a.DeviationPct, a.PhaseShiftDeg,
					lenComplex(a.ZBaseline), lenComplex(a.ZMeasured))
			}
		}
	}

	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		fmt.Fprintf(os.Stderr, "\nWARNING: gateway exited with error: %v\n", err)
		os.Exit(2)
	}
}

func lenComplex(c complex128) float64 {
	r := real(c)
	i := imag(c)
	return math.Sqrt(r*r + i*i)
}
