//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/mattn/go-isatty"
	"github.com/will25u/slimmedia-agent/internal/daemon"
	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
	"github.com/will25u/slimmedia-agent/internal/safety"
	"github.com/will25u/slimmedia-agent/internal/scanner"
	"github.com/will25u/slimmedia-agent/internal/transport"
)

// auditConfigFallback fills in the server/API key from the saved config
// under the per-user state dir when the audit flags did not supply them.
func auditConfigFallback(saasServer, saasAPIKey string) (string, string) {
	if saasServer != "" && saasAPIKey != "" {
		return saasServer, saasAPIKey
	}
	if savedCfg, err := daemon.LoadSavedConfig(); err == nil {
		if saasServer == "" && savedCfg.Server != "" {
			saasServer = savedCfg.Server
		}
		if saasAPIKey == "" && savedCfg.APIKey != "" {
			saasAPIKey = savedCfg.APIKey
		}
	}
	return saasServer, saasAPIKey
}

func runAudit(targetDir, saasServer, saasAPIKey string, insecure bool) {
	const (
		cReset   = "\033[0m"
		cBold    = "\033[1m"
		cDim     = "\033[2m"
		cGreen   = "\033[32m"
		cYellow  = "\033[33m"
		cBlue    = "\033[34m"
		cMagenta = "\033[35m"
		cCyan    = "\033[36m"
		cWhite   = "\033[37m"
		cGray    = "\033[90m"
	)

	formatCodecBadge := func(c string) string {
		switch strings.ToLower(c) {
		case "h264", "avc":
			return fmt.Sprintf("%s[H.264]%s", cCyan+cBold, cReset)
		case "hevc", "h265":
			return fmt.Sprintf("%s[HEVC]%s", cGreen+cBold, cReset)
		case "mpeg2", "mpeg2video":
			return fmt.Sprintf("%s[MPEG-2]%s", cYellow+cBold, cReset)
		case "vc1", "wmv":
			return fmt.Sprintf("%s[VC-1]%s", cMagenta+cBold, cReset)
		case "av1":
			return fmt.Sprintf("%s[AV1]%s", cGreen+cBold, cReset)
		default:
			return fmt.Sprintf("%s[%s]%s", cBlue+cBold, strings.ToUpper(c), cReset)
		}
	}

	formatHDRBadge := func(hdr string) string {
		switch strings.ToUpper(hdr) {
		case "DV":
			return fmt.Sprintf("%s[DV]%s", cMagenta+cBold, cReset)
		case "HDR10", "HDR":
			return fmt.Sprintf("%s[%s]%s", cYellow+cBold, strings.ToUpper(hdr), cReset)
		case "HLG":
			return fmt.Sprintf("%s[HLG]%s", cCyan+cBold, cReset)
		default:
			return fmt.Sprintf("%s[SDR]%s", cGray, cReset)
		}
	}

	formatAudioBadge := func(audio string) string {
		if audio == "" {
			return fmt.Sprintf("%s[No Audio]%s", cGray, cReset)
		}
		return fmt.Sprintf("%s[%s]%s", cBlue+cBold, audio, cReset)
	}

	formatBitrate := func(br int64) string {
		if br <= 0 {
			return fmt.Sprintf("%s%10s%s", cGray, "-", cReset)
		}
		mbps := float64(br) / 1_000_000.0
		if mbps >= 1.0 {
			return fmt.Sprintf("%s%6.1f Mbps%s", cWhite, mbps, cReset)
		}
		return fmt.Sprintf("%s%6d kbps%s", cWhite, br/1000, cReset)
	}

	isInteractive := isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
	if isInteractive {
		fmt.Print("\033[2J\033[H")
	}

	fmt.Printf("%s%s=== SlimMedia Phase 1 Audit ===%s\n", cCyan, cBold, cReset)
	fmt.Printf("Target directory: %s%s%s\n\n", cWhite, targetDir, cReset)

	dataDir := daemon.StateDir()
	os.MkdirAll(dataDir, 0700)
	_ = os.Chmod(dataDir, 0700)

	fmt.Printf("%s•%s Ensuring FFmpeg engine is available... ", cCyan, cReset)
	if err := ffmpeg.EnsureFFmpeg(dataDir); err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}
	fmt.Printf("%sready%s\n", cGreen, cReset)

	ffpathForCaps := ffmpeg.ResolveBinaryPath(dataDir, "ffmpeg")
	fmt.Printf("%s•%s Probing FFmpeg encoder capabilities... ", cCyan, cReset)
	encCaps := ffmpeg.ProbeEncoders(ffpathForCaps)
	fmt.Printf("%sdone%s\n", cGreen, cReset)

	caps := hardware.Probe()

	assessment := daemon.DetermineTier(caps, encCaps)
	tier := assessment.Tier
	tierMsg := assessment.Message
	tierColor := cYellow
	if tier == daemon.TierExcellent {
		tierColor = cGreen
	} else if tier == daemon.TierGood {
		tierColor = cCyan
	}

	cNvidia := "\033[38;2;118;185;0m" // #76B900 NVIDIA Green
	cIntel := "\033[38;2;0;113;197m"  // #0071C5 Intel Blue
	cAMD := "\033[38;2;237;28;36m"    // #ED1C24 AMD Red

	avx2Val := cGray
	if caps.CPU.HasAVX2 {
		avx2Val = cWhite + cBold
	}
	avxVal := cGray
	if caps.CPU.HasAVX {
		avxVal = cWhite + cBold
	}
	nvidiaVal := cGray
	if caps.GPU.HasNvidia {
		nvidiaVal = cWhite + cBold
	}
	intelVal := cGray
	if caps.GPU.HasIntel {
		intelVal = cWhite + cBold
	}
	amdVal := cGray
	if caps.GPU.HasAMD {
		amdVal = cWhite + cBold
	}

	// Disk probe for target directory
	diskInfo := hardware.ProbeDisk(targetDir)

	fmt.Printf("\n%s--- Hardware Report ---%s\n", cBold, cReset)
	if caps.OS.Name != "" {
		fmt.Printf("OS:             %s%s%s\n", cWhite, caps.OS.Name, cReset)
	}
	cpuModel := caps.CPU.Model
	if cpuModel == "" {
		cpuModel = caps.CPU.Arch
	}
	fmt.Printf("CPU:            %s%s%s\n", cWhite, cpuModel, cReset)
	fmt.Printf("  Architecture: %s%s%s\n", cWhite, caps.CPU.Arch, cReset)
	if caps.CPU.PhysicalCores > 0 {
		fmt.Printf("  Cores:        %s%d physical%s / %s%d logical (HT)%s\n",
			cWhite+cBold, caps.CPU.PhysicalCores, cReset,
			cWhite, caps.CPU.LogicalCores, cReset)
	} else {
		fmt.Printf("  Logical Cores:%s%d%s\n", cWhite+cBold, caps.CPU.LogicalCores, cReset)
	}
	fmt.Printf("  SIMD:         AVX: %s%t%s  AVX2: %s%t%s\n",
		avxVal, caps.CPU.HasAVX, cReset,
		avx2Val, caps.CPU.HasAVX2, cReset)
	if caps.Memory.TotalGB > 0 {
		fmt.Printf("RAM:            %s%.1f GB total%s\n", cWhite+cBold, caps.Memory.TotalGB, cReset)
	}
	if len(diskInfo) > 0 {
		d := diskInfo[0]
		usedDiskGB := d.TotalGB - d.FreeGB
		fmt.Printf("Media Disk:     %s%.1f GB total%s  %s%.1f GB used%s  %s%.1f GB free%s\n",
			cWhite+cBold, d.TotalGB, cReset,
			cYellow, usedDiskGB, cReset,
			cGreen, d.FreeGB, cReset)
	}
	gpuModel := caps.GPU.Model
	if gpuModel == "" {
		gpuModel = "None detected"
	}
	fmt.Printf("GPU:            %s%s%s\n", cWhite, gpuModel, cReset)
	if caps.GPU.Architecture != "" {
		fmt.Printf("  Architecture: %s%s%s\n", cWhite, caps.GPU.Architecture, cReset)
	}
	if caps.GPU.TotalMemoryMB > 0 {
		if caps.GPU.SharedSystemMemMB > 0 {
			fmt.Printf("  Memory:       %s%d MB dedicated%s / %s%d MB shared%s (%d MB total)\n",
				cWhite+cBold, caps.GPU.DedicatedVRAMMB, cReset,
				cWhite, caps.GPU.SharedSystemMemMB, cReset,
				caps.GPU.TotalMemoryMB)
		} else {
			fmt.Printf("  VRAM:         %s%d MB dedicated%s\n", cWhite+cBold, caps.GPU.DedicatedVRAMMB, cReset)
		}
	}
	fmt.Printf("  HW Encoders:  %sNvidia:%s %s%t%s  %sIntel:%s %s%t%s  %sAMD:%s %s%t%s\n",
		cNvidia+cBold, cReset, nvidiaVal, caps.GPU.HasNvidia, cReset,
		cIntel+cBold, cReset, intelVal, caps.GPU.HasIntel, cReset,
		cAMD+cBold, cReset, amdVal, caps.GPU.HasAMD, cReset)
	fmt.Printf("Viability Tier: %s%s%s — %s\n", tierColor+cBold, tier, cReset, tierMsg)

	fmt.Printf("\n%s--- Media Scan ---%s\n", cBold, cReset)

	var files []string
	var err error

	if isInteractive {
		var foundCount atomic.Int64
		doneDiscovering := make(chan struct{})
		spinnerFrames := []string{"|", "/", "-", "\\"}
		go func() {
			idx := 0
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-doneDiscovering:
					return
				case <-ticker.C:
					frame := spinnerFrames[idx%len(spinnerFrames)]
					idx++
					count := foundCount.Load()
					fmt.Printf("\033[2K\r%s•%s Discovering media files in %s... %s[%s]%s (Found %d so far)",
						cCyan, cReset, targetDir, cYellow+cBold, frame, cReset, count)
				}
			}
		}()

		files, err = scanner.WalkWithProgress(targetDir, func(path string, totalFound int) {
			foundCount.Store(int64(totalFound))
		})
		close(doneDiscovering)
		fmt.Print("\033[2K\r")
	} else {
		fmt.Printf("%s•%s Discovering media files in %s (traversing folders)...\n", cCyan, cReset, targetDir)
		files, err = scanner.Walk(targetDir)
	}

	if err != nil {
		fmt.Printf("Scanner failed: %v\n", err)
		return
	}

	totalFiles := len(files)
	if totalFiles == 0 {
		fmt.Printf("%sNo supported media files found in directory.%s\n", cYellow, cReset)
		return
	}
	ctrlCfg := scanner.DefaultAdaptiveConfig()
	if ctrlCfg.MaxWorkers > totalFiles {
		ctrlCfg.MaxWorkers = totalFiles
	}
	ctrl := scanner.NewAdaptiveController(ctrlCfg)

	fmt.Printf("%s•%s Discovered %s%d%s media file(s). Probing metadata with dynamic adaptive concurrency (1-%s%d%s workers)...\n",
		cCyan, cReset, cWhite+cBold, totalFiles, cReset, cCyan+cBold, ctrlCfg.MaxWorkers, cReset)

	ffprobePath := filepath.Join(dataDir, "ffprobe")

	var totalSize, totalSavings int64
	var totalDuration, transcodeableDuration float64
	var transcodeableProfiles []FileTranscodeProfile
	var analyzed int
	codecCounts := make(map[string]int)
	var recentFiles []string
	renderedBefore := false

	// --- Keyboard control (interactive only) ---
	var quitFlag, pausedFlag, saveFlag atomic.Bool

	if isInteractive {
		// Put stdin into raw mode so we can read single keypresses
		var oldTermios syscall.Termios
		if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&oldTermios)), 0, 0, 0); errno == 0 {
			raw := oldTermios
			raw.Lflag &^= syscall.ECHO | syscall.ICANON
			raw.Cc[syscall.VMIN] = 1
			raw.Cc[syscall.VTIME] = 0
			syscall.Syscall6(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&raw)), 0, 0, 0)
			// Restore on exit
			defer syscall.Syscall6(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&oldTermios)), 0, 0, 0)
		}
		go func() {
			buf := make([]byte, 1)
			for {
				n, err := os.Stdin.Read(buf)
				if err != nil || n == 0 {
					return
				}
				switch buf[0] | 0x20 { // lowercase
				case 'q':
					quitFlag.Store(true)
					return
				case 'p':
					pausedFlag.Store(!pausedFlag.Load())
				case 's':
					saveFlag.Store(true)
				}
			}
		}()
	}

	formatDuration := func(d time.Duration) string {
		d = d.Round(time.Second)
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		s := int(d.Seconds()) % 60
		if h > 0 {
			return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
		}
		return fmt.Sprintf("%02d:%02d", m, s)
	}

	type probeResult struct {
		file          string
		info          ffmpeg.MediaInfo
		err           error
		activeWorkers int
		maxWorkers    int
	}

	tasks := make(chan string, totalFiles)
	for _, f := range files {
		tasks <- f
	}
	close(tasks)

	results := make(chan probeResult, totalFiles)
	limiter := scanner.NewAdaptiveLimiter(ctrl.CurrentWorkers())
	var wg sync.WaitGroup

	numWorkers := ctrlCfg.MaxWorkers
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range tasks {
				limiter.Acquire()
				activeNow := limiter.CurrentActive()

				start := time.Now()
				info, err := ffmpeg.ProbeMedia(ffprobePath, f)
				duration := time.Since(start)

				target := ctrl.ReportObservation(duration, err != nil)
				limiter.SetLimit(target)
				limiter.Release()

				results <- probeResult{
					file:          f,
					info:          info,
					err:           err,
					activeWorkers: activeNow,
					maxWorkers:    ctrlCfg.MaxWorkers,
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	scanStartTime := time.Now()
	completed := 0

	for res := range results {
		// Handle keyboard quit
		if quitFlag.Load() {
			break
		}
		// Handle keyboard pause — spin until unpaused or quit
		for pausedFlag.Load() {
			if quitFlag.Load() {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if quitFlag.Load() {
			break
		}

		completed++
		f := res.file
		info := res.info
		err := res.err

		baseName := filepath.Base(f)
		displayName := baseName
		if len(displayName) > 28 {
			displayName = displayName[:25] + "..."
		}

		elapsed := time.Since(scanStartTime)
		var eta time.Duration
		if completed > 1 {
			remaining := totalFiles - completed
			eta = time.Duration(float64(elapsed) / float64(completed) * float64(remaining))
		}
		elapsedStr := formatDuration(elapsed)
		etaStr := "--:--"
		if completed > 1 {
			etaStr = formatDuration(eta)
		}

		if err != nil {
			if isInteractive {
				recentFiles = append(recentFiles, fmt.Sprintf("  • %s%-28s%s  %s%8s%s  %s%10s%s  %s%-10s%s", cWhite, displayName, cReset, cGray, "-", cReset, cGray, "-", cReset, cGray, "(probe error)", cReset))
			} else {
				fmt.Printf("[%d/%d] %s - probe error [Concurrency: %d/%d, Elapsed: %s, ETA: %s]\n", completed, totalFiles, displayName, res.activeWorkers, res.maxWorkers, elapsedStr, etaStr)
			}
		} else {
			analyzed++
			totalSize += info.Size
			totalDuration += info.Duration
			codec := info.Codec
			if codec == "" {
				codec = "unknown"
			}
			codecCounts[codec]++

			var savingsRatio float64
			if codec == "h264" {
				savingsRatio = 0.40
			} else if codec == "mpeg2video" || codec == "mpeg2" {
				savingsRatio = 0.65
			} else if codec == "hevc" || codec == "h265" {
				savingsRatio = 0.0
			} else {
				savingsRatio = 0.30
			}
			totalSavings += int64(float64(info.Size) * savingsRatio)
			if savingsRatio > 0 {
				transcodeableDuration += info.Duration
				transcodeableProfiles = append(transcodeableProfiles, FileTranscodeProfile{
					Resolution:  info.Resolution,
					HDR:         info.HDR,
					Duration:    info.Duration,
					TotalFrames: info.TotalFrames,
				})
			}

			sizeGB := float64(info.Size) / (1024 * 1024 * 1024)
			var sizeStr string
			if sizeGB >= 1.0 {
				sizeStr = fmt.Sprintf("%.2f GB", sizeGB)
			} else {
				sizeStr = fmt.Sprintf("%.1f MB", float64(info.Size)/(1024*1024))
			}

			if isInteractive {
				recentFiles = append(recentFiles, fmt.Sprintf("  • %s%-28s%s  %s%8s%s  %s  %-15s %-10s %s",
					cWhite, displayName, cReset,
					cCyan, sizeStr, cReset,
					formatBitrate(info.Bitrate),
					formatCodecBadge(codec),
					formatHDRBadge(info.HDR),
					formatAudioBadge(info.AudioStream)))
			} else {
				audioStr := info.AudioStream
				if audioStr == "" {
					audioStr = "no audio"
				}
				fmt.Printf("[%d/%d] %s (%s, %s, %s, %s, %s) [Concurrency: %d/%d, Elapsed: %s, ETA: %s]\n",
					completed, totalFiles, displayName, sizeStr, strings.TrimSpace(formatBitrate(info.Bitrate)), codec, info.HDR, audioStr, res.activeWorkers, res.maxWorkers, elapsedStr, etaStr)
			}
		}

		if len(recentFiles) > 5 {
			recentFiles = recentFiles[len(recentFiles)-5:]
		}

		if isInteractive {
			if renderedBefore {
				fmt.Print("\033[u") // restore saved cursor position (wrap-safe)
			} else {
				fmt.Print("\033[s") // save cursor position before first render
			}
			renderedBefore = true

			currentSizeGB := float64(totalSize) / (1024 * 1024 * 1024)
			currentSavingsGB := float64(totalSavings) / (1024 * 1024 * 1024)
			pct := (float64(completed) / float64(totalFiles)) * 100

			// Line 1: counters
			fmt.Printf("\033[2K\r%sProgress:%s %s[%d/%d]%s %s(%.1f%%)%s %s[%d/%d threads]%s | %sElapsed:%s %s%s%s | %sETA:%s %s%s%s\n",
				cBold, cReset,
				cCyan+cBold, completed, totalFiles, cReset,
				cDim, pct, cReset,
				cWhite, res.activeWorkers, res.maxWorkers, cReset,
				cBold, cReset, cWhite+cBold, elapsedStr, cReset,
				cBold, cReset, cYellow+cBold, etaStr, cReset)
			// Line 2: size stats
			fmt.Printf("\033[2K\r%sSize:%s %s%.2f GB%s  %sSavings:%s %s%.2f GB%s\n",
				cBold, cReset, cGreen+cBold, currentSizeGB, cReset,
				cBold, cReset, cYellow+cBold, currentSavingsGB, cReset)

			var codecKeys []string
			for k := range codecCounts {
				codecKeys = append(codecKeys, k)
			}
			sort.Strings(codecKeys)
			var codecParts []string
			for _, k := range codecKeys {
				codecParts = append(codecParts, fmt.Sprintf("%s: %s%d%s", formatCodecBadge(k), cWhite+cBold, codecCounts[k], cReset))
			}
			codecLine := "none"
			if len(codecParts) > 0 {
				codecLine = strings.Join(codecParts, " | ")
			}
			fmt.Printf("\033[2K\r%sCodecs:  %s %s\n", cBold, cReset, codecLine)

			fmt.Printf("\033[2K\r%sRecent Files (Last 5):%s\n", cBold, cReset)
			for slot := 0; slot < 5; slot++ {
				if slot < len(recentFiles) {
					fmt.Printf("\033[2K\r%s\n", recentFiles[slot])
				} else {
					fmt.Print("\033[2K\r\n")
				}
			}

			// Legend
			pauseLabel := "Pause"
			if pausedFlag.Load() {
				pauseLabel = cYellow + cBold + "PAUSED" + cReset
			}
			fmt.Printf("\033[2K\r%s─────────────────────────────────────────────────────%s\n", cGray, cReset)
			fmt.Printf("\033[2K\r%s[Q]%s Quit  %s[P]%s %s  %s[S]%s Save partial report\n",
				cCyan+cBold, cReset, cCyan+cBold, cReset, pauseLabel, cCyan+cBold, cReset)

			// Handle save flag
			if saveFlag.Load() {
				saveFlag.Store(false)
				partialReport := daemon.AuditReport{}
				partialReport.Hardware.CPU.Arch = caps.CPU.Arch
				partialReport.Hardware.CPU.HasAVX2 = caps.CPU.HasAVX2
				partialReport.Hardware.GPU.Model = caps.GPU.Model
				partialReport.Hardware.GPU.HasNvidia = caps.GPU.HasNvidia
				partialReport.Hardware.GPU.HasIntel = caps.GPU.HasIntel
				partialReport.Hardware.GPU.HasAMD = caps.GPU.HasAMD
				partialReport.Hardware.Tier = string(tier)
				partialReport.Media.FilesAnalyzed = analyzed
				partialReport.Media.TotalSizeGB = float64(totalSize) / (1024 * 1024 * 1024)
				partialReport.Media.EstimatedSavingsGB = float64(totalSavings) / (1024 * 1024 * 1024)
				partialReport.Media.CodecBreakdown = codecCounts
				partEst := CalculateLibraryEstimate(caps, encCaps, transcodeableProfiles)
				partialReport.Media.EstimatedTranscodeHours = partEst.TotalTranscodeSecs / 3600.0
				if out, err := json.MarshalIndent(partialReport, "", "  "); err == nil {
					os.WriteFile(filepath.Join(daemon.StateDir(), "report.json"), out, 0640)
				}
			}
		}
	}

	totalSizeGB := float64(totalSize) / (1024 * 1024 * 1024)
	totalSavingsGB := float64(totalSavings) / (1024 * 1024 * 1024)

	var savingsPercent float64
	if totalSize > 0 {
		savingsPercent = (float64(totalSavings) / float64(totalSize)) * 100
	}

	totalElapsed := time.Since(scanStartTime)
	totalScanTimeStr := formatDuration(totalElapsed)

	libEstimate := CalculateLibraryEstimate(caps, encCaps, transcodeableProfiles)
	estTranscodeSecs := libEstimate.TotalTranscodeSecs
	estTranscodeHours := estTranscodeSecs / 3600.0
	hwNote := libEstimate.HWNote

	var estTranscodeStr string
	if transcodeableDuration == 0 {
		estTranscodeStr = "0 hours (all files already optimized)"
	} else if estTranscodeHours >= 1.0 {
		estTranscodeStr = fmt.Sprintf("~%.1f hours %s", estTranscodeHours, hwNote)
	} else if estTranscodeSecs >= 60 {
		estTranscodeStr = fmt.Sprintf("~%d minutes %s", int(estTranscodeSecs/60), hwNote)
	} else {
		estTranscodeStr = fmt.Sprintf("~%d seconds %s", int(estTranscodeSecs), hwNote)
	}

	fmt.Println()
	fmt.Printf("%s=== %sSCAN COMPLETE%s %s========================================%s\n", cCyan, cBold+cGreen, cReset, cCyan, cReset)
	fmt.Printf("  %sScan Duration:%s        %s%s%s\n", cBold, cReset, cWhite+cBold, totalScanTimeStr, cReset)
	fmt.Printf("  %sFiles Analyzed:%s       %s%d%s\n", cBold, cReset, cWhite+cBold, analyzed, cReset)
	fmt.Printf("  %sTotal Library Size:%s   %s%.2f GB%s\n", cBold, cReset, cCyan+cBold, totalSizeGB, cReset)
	fmt.Printf("  %sEstimated Savings:%s    %s%.2f GB (%.1f%%)%s\n", cBold, cReset, cYellow+cBold, totalSavingsGB, savingsPercent, cReset)
	fmt.Printf("  %sTotal Playtime:%s       %s%.1f hours%s\n", cBold, cReset, cWhite, totalDuration/3600.0, cReset)
	fmt.Printf("  %sEst. Transcode Time:%s  %s%s%s\n", cBold, cReset, cGreen+cBold, estTranscodeStr, cReset)
	if transcodeableDuration > 0 && len(libEstimate.ByResolution) > 0 {
		var resKeys []string
		for r := range libEstimate.ByResolution {
			resKeys = append(resKeys, r)
		}
		sort.Slice(resKeys, func(i, j int) bool {
			order := map[string]int{"4K": 1, "1080p": 2, "720p": 3, "SD": 4}
			return order[resKeys[i]] < order[resKeys[j]]
		})
		for _, r := range resKeys {
			re := libEstimate.ByResolution[r]
			hours := re.TranscodeSecs / 3600.0
			var timeStr string
			if hours >= 1.0 {
				timeStr = fmt.Sprintf("~%.1f hours", hours)
			} else if re.TranscodeSecs >= 60 {
				timeStr = fmt.Sprintf("~%d minutes", int(re.TranscodeSecs/60))
			} else {
				timeStr = fmt.Sprintf("~%d seconds", int(re.TranscodeSecs))
			}
			fmt.Printf("    • %-10s %s%s%s %s(%d file(s), ~%.0f FPS avg)%s\n",
				r+":", cWhite+cBold, timeStr, cReset, cGray, re.Count, re.AvgFPS, cReset)
		}
	}
	if len(codecCounts) > 0 {
		var keys []string
		for k := range codecCounts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("\n  %sCodec Breakdown:%s\n", cBold, cReset)
		for _, k := range keys {
			fmt.Printf("    • %-18s %s%d file(s)%s\n", formatCodecBadge(k), cWhite, codecCounts[k], cReset)
		}
	}
	fmt.Printf("%s===========================================================%s\n", cCyan, cReset)

	// --- Build full report (hardware + FFmpeg + media-server sections are
	// shared with the daemon's automatic pre-connect capabilities report) ---
	report := daemon.BuildCapabilitiesReport(caps, encCaps, safety.ProbeMediaServerPresence())

	// Hardware — Media disk
	if diskInfo := hardware.ProbeDisk(targetDir); len(diskInfo) > 0 {
		report.Hardware.MediaDisk.Mount = diskInfo[0].Mount
		report.Hardware.MediaDisk.TotalGB = diskInfo[0].TotalGB
		report.Hardware.MediaDisk.FreeGB = diskInfo[0].FreeGB
	}

	// Media
	report.Media.TargetDirectory = targetDir
	report.Media.FilesAnalyzed = analyzed
	report.Media.TotalSourceDurationHrs = totalDuration / 3600.0
	report.Media.TotalSizeGB = totalSizeGB
	report.Media.EstimatedSavingsGB = totalSavingsGB
	report.Media.EstimatedSavingsPercent = savingsPercent
	report.Media.EstimatedTranscodeHours = estTranscodeHours
	report.Media.ScanElapsedSeconds = totalElapsed.Seconds()
	report.Media.CodecBreakdown = codecCounts

	// --- Save locally ---
	out, _ := json.MarshalIndent(report, "", "  ")
	reportPath := filepath.Join(daemon.StateDir(), "report.json")
	os.WriteFile(reportPath, out, 0640)
	fmt.Printf("\nReport saved to %s\n", reportPath)

	// --- POST to SaaS if credentials available ---
	if saasServer != "" && saasAPIKey != "" {
		apiURL, _, err := transport.ResolveServerURLs(saasServer, insecure)
		if err != nil {
			// The report is already saved locally; refuse to leak the API
			// key to a plaintext non-loopback endpoint rather than abort.
			fmt.Printf("\n%s• Skipping report upload: %v%s\n", cYellow, err, cReset)
			return
		}
		endpoint := apiURL + "/api/v1/agents/audit-report"
		fmt.Printf("\n%s•%s Reporting results to %s... ", cCyan, cReset, apiURL)

		client := &http.Client{Timeout: 30 * time.Second}
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(out))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", saasAPIKey)
			resp, err := client.Do(req)
			if err != nil {
				fmt.Printf("%sFailed: %v%s\n", cYellow, err, cReset)
			} else {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent {
					fmt.Printf("%ssent ✓%s\n", cGreen, cReset)
				} else {
					fmt.Printf("%sFailed (HTTP %d)%s\n", cYellow, resp.StatusCode, cReset)
				}
			}
		}
	} else {
		fmt.Printf("%s(Tip: run with --server and --api-key to upload results to your SlimMedia dashboard)%s\n", cGray, cReset)
	}
}
