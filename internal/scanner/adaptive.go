package scanner

import (
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
)

// AdaptiveConfig tunes the conservative feedback loop for media scanning concurrency.
type AdaptiveConfig struct {
	MinWorkers       int
	MaxWorkers       int
	InitialWorkers   int
	TargetLatency    time.Duration // Desired maximum probe latency before throttling
	FastLatency      time.Duration // Latency threshold considered fast enough to scale up
	ScaleUpStreak    int           // Number of consecutive fast probes required to add a worker
	CPULoadThreshold float64       // Load average per CPU core threshold (e.g. 0.7 = 70%)
}

// DefaultAdaptiveConfig returns a very conservative configuration to avoid saturating
// CPU, network links, or disk I/O queues.
func DefaultAdaptiveConfig() AdaptiveConfig {
	numCPU := runtime.NumCPU()
	maxWorkers := numCPU / 2
	if maxWorkers < 2 {
		maxWorkers = 2
	}
	if maxWorkers > 4 {
		maxWorkers = 4 // Very conservative hard cap to protect NAS/disks/CPU
	}

	return AdaptiveConfig{
		MinWorkers:       1,
		MaxWorkers:       maxWorkers,
		InitialWorkers:   1,
		TargetLatency:    1500 * time.Millisecond,
		FastLatency:      400 * time.Millisecond,
		ScaleUpStreak:    3,
		CPULoadThreshold: 0.75,
	}
}

// AdaptiveController dynamically calculates optimal worker concurrency.
type AdaptiveController struct {
	mu             sync.Mutex
	cfg            AdaptiveConfig
	currentWorkers int
	fastStreak     int
	lastLatency    time.Duration
	smoothedLat    time.Duration
}

// NewAdaptiveController initializes a controller with the given config.
func NewAdaptiveController(cfg AdaptiveConfig) *AdaptiveController {
	if cfg.MinWorkers <= 0 {
		cfg.MinWorkers = 1
	}
	if cfg.MaxWorkers < cfg.MinWorkers {
		cfg.MaxWorkers = cfg.MinWorkers
	}
	initial := cfg.InitialWorkers
	if initial < cfg.MinWorkers {
		initial = cfg.MinWorkers
	}
	if initial > cfg.MaxWorkers {
		initial = cfg.MaxWorkers
	}

	return &AdaptiveController{
		cfg:            cfg,
		currentWorkers: initial,
	}
}

// CurrentWorkers returns the current target concurrency.
func (c *AdaptiveController) CurrentWorkers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentWorkers
}

// ReportObservation adjusts concurrency based on probe execution duration and success.
func (c *AdaptiveController) ReportObservation(duration time.Duration, hasError bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastLatency = duration
	if c.smoothedLat == 0 {
		c.smoothedLat = duration
	} else {
		// EWMA: 70% previous, 30% current
		c.smoothedLat = time.Duration(0.7*float64(c.smoothedLat) + 0.3*float64(duration))
	}

	// Immediate backoff if error encountered (e.g. network timeout or I/O failure)
	if hasError {
		c.currentWorkers = c.cfg.MinWorkers
		c.fastStreak = 0
		return c.currentWorkers
	}

	// Check CPU load if available
	cpuBusy := c.isCPUConstrained()

	// Severe latency spike or CPU overloaded -> immediate scale down
	if duration > c.cfg.TargetLatency || c.smoothedLat > c.cfg.TargetLatency || cpuBusy {
		if c.currentWorkers > c.cfg.MinWorkers {
			c.currentWorkers--
		}
		c.fastStreak = 0
		return c.currentWorkers
	}

	// Responsive I/O and low CPU -> cautiously build streak towards scale-up
	if c.smoothedLat <= c.cfg.FastLatency && !cpuBusy {
		c.fastStreak++
		if c.fastStreak >= c.cfg.ScaleUpStreak {
			if c.currentWorkers < c.cfg.MaxWorkers {
				c.currentWorkers++
			}
			c.fastStreak = 0
		}
	} else {
		// Neutral zone: keep current workers, decay streak
		if c.fastStreak > 0 {
			c.fastStreak--
		}
	}

	return c.currentWorkers
}

// isCPUConstrained checks system load average relative to available cores.
func (c *AdaptiveController) isCPUConstrained() bool {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return false // Non-Linux or restricted procfs; fallback to latency metric
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return false
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return false
	}
	cores := float64(runtime.NumCPU())
	if cores <= 0 {
		cores = 1
	}
	return (load1 / cores) > c.cfg.CPULoadThreshold
}

// AdaptiveLimiter dynamically restricts concurrency using condition variable signaling.
type AdaptiveLimiter struct {
	mu      sync.Mutex
	cond    *sync.Cond
	current int
	limit   int
}

// NewAdaptiveLimiter creates a new limiter with the given initial limit.
func NewAdaptiveLimiter(initialLimit int) *AdaptiveLimiter {
	l := &AdaptiveLimiter{
		limit: initialLimit,
	}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// Acquire blocks until a slot is available under the current dynamic limit.
func (l *AdaptiveLimiter) Acquire() {
	l.mu.Lock()
	for l.current >= l.limit {
		l.cond.Wait()
	}
	l.current++
	l.mu.Unlock()
}

// Release frees a concurrency slot and notifies waiting workers.
func (l *AdaptiveLimiter) Release() {
	l.mu.Lock()
	if l.current > 0 {
		l.current--
	}
	l.cond.Broadcast()
	l.mu.Unlock()
}

// SetLimit adjusts the dynamic concurrency limit and signals any waiting workers.
func (l *AdaptiveLimiter) SetLimit(limit int) {
	l.mu.Lock()
	if limit < 1 {
		limit = 1
	}
	l.limit = limit
	l.cond.Broadcast()
	l.mu.Unlock()
}

// CurrentActive returns the number of currently active concurrency slots.
func (l *AdaptiveLimiter) CurrentActive() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current
}

// ProbeCallback reports progress, current dynamic concurrency, and max concurrency per probed file.
type ProbeCallback func(file string, info ffmpeg.MediaInfo, currentConcurrency int, maxConcurrency int, completed int, total int)

// AdaptiveScanFiles probes a list of file paths using real-time adaptive concurrency,
// streaming through a bounded channel rather than holding unbounded task queues in heap memory.
func AdaptiveScanFiles(paths []string, ffprobePath string, cb ProbeCallback) ([]ScannedFile, error) {
	total := len(paths)
	if total == 0 {
		return nil, nil
	}

	queue := make(chan string, DefaultScanChannelCapacity)
	go func() {
		defer close(queue)
		for _, p := range paths {
			queue <- p
		}
	}()

	return AdaptiveScanStream(queue, ffprobePath, cb, total)
}

// AdaptiveScanStream probes a stream of file paths from a channel using real-time adaptive concurrency.
func AdaptiveScanStream(paths <-chan string, ffprobePath string, cb ProbeCallback, totalCount ...int) ([]ScannedFile, error) {
	total := 0
	if len(totalCount) > 0 {
		total = totalCount[0]
	}

	cfg := DefaultAdaptiveConfig()
	if total > 0 && cfg.MaxWorkers > total {
		cfg.MaxWorkers = total
	}
	ctrl := NewAdaptiveController(cfg)
	limiter := NewAdaptiveLimiter(ctrl.CurrentWorkers())

	var results []ScannedFile
	if total > 0 {
		results = make([]ScannedFile, 0, total)
	}
	var resultsMu sync.Mutex
	var completedCount int
	var countMu sync.Mutex
	var wg sync.WaitGroup

	numWorkers := cfg.MaxWorkers
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range paths {
				limiter.Acquire()
				activeNow := limiter.CurrentActive()

				filename := filepathBase(p)

				start := time.Now()
				var info ffmpeg.MediaInfo
				var probeErr error

				if ffprobePath != "" {
					info, probeErr = ffmpeg.ProbeMedia(ffprobePath, p)
				}

				duration := time.Since(start)

				target := ctrl.ReportObservation(duration, probeErr != nil)
				limiter.SetLimit(target)

				codec := info.Codec
				fileDuration := info.Duration
				size := info.Size

				if size == 0 {
					if fi, statErr := os.Stat(p); statErr == nil {
						size = fi.Size()
					}
				}
				if codec == "" {
					codec = "unknown"
				}

				var item ScannedFile
				if probeErr == nil {
					hash, err := Fingerprint(p)
					if err != nil {
						log.Printf("Fingerprint failed for %s: %v", p, err)
					}

					item = ScannedFile{
						FilePath:  p,
						Filename:  filename,
						Codec:     codec,
						Duration:  fileDuration,
						SizeBytes: size,
						Hash:      hash,
					}
				} else {
					log.Printf("scanner: probe failed for %s: %v", p, probeErr)
					item = ScannedFile{
						FilePath:  p,
						Filename:  filename,
						Codec:     "error",
						SizeBytes: size,
						Error:     probeErr.Error(),
					}
				}

				limiter.Release()

				countMu.Lock()
				completedCount++
				currentCompleted := completedCount
				if total > 0 && currentCompleted > total {
					total = currentCompleted
				}
				countMu.Unlock()

				resultsMu.Lock()
				results = append(results, item)
				resultsMu.Unlock()

				if cb != nil {
					cb(p, info, activeNow, cfg.MaxWorkers, currentCompleted, total)
				}
			}
		}()
	}

	wg.Wait()
	return results, nil
}

func filepathBase(p string) string {
	idx := strings.LastIndex(p, string(os.PathSeparator))
	if idx >= 0 {
		return p[idx+1:]
	}
	return p
}
