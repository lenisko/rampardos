package services

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

const (
	maxQueueSize   = 50000
	touchInterval  = 30 * time.Second
	touchBatchSize = 75
)

// FileToucher batches file touch operations
type FileToucher struct {
	mu     sync.Mutex
	queue  []string
	ctx    context.Context
	cancel context.CancelFunc
}

// NewFileToucher creates a new file toucher
func NewFileToucher() *FileToucher {
	ctx, cancel := context.WithCancel(context.Background())
	ft := &FileToucher{
		queue:  make([]string, 0, 1000),
		ctx:    ctx,
		cancel: cancel,
	}
	return ft
}

// Start begins the background touch loop
func (ft *FileToucher) Start() {
	go ft.run()
}

// Stop stops the file toucher
func (ft *FileToucher) Stop() {
	ft.cancel()
}

// Touch queues a file to be touched
func (ft *FileToucher) Touch(fileName string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	if len(ft.queue) >= maxQueueSize {
		slog.Warn("FileToucher queue full, dropping touch request")
		return
	}
	ft.queue = append(ft.queue, fileName)
}

func (ft *FileToucher) run() {
	ticker := time.NewTicker(touchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ft.ctx.Done():
			return
		case <-ticker.C:
			ft.runOnce()
		}
	}
}

func (ft *FileToucher) runOnce() {
	ft.mu.Lock()
	currentQueue := ft.queue
	ft.queue = make([]string, 0, 1000)
	ft.mu.Unlock()

	// Report queue size to metrics
	if GlobalMetrics != nil {
		GlobalMetrics.SetFileToucherQueueSize(len(currentQueue))
	}

	if len(currentQueue) == 0 {
		return
	}

	count := 0
	now := time.Now()

	// Process in batches
	for i := 0; i < len(currentQueue); i += touchBatchSize {
		end := i + touchBatchSize
		if end > len(currentQueue) {
			end = len(currentQueue)
		}
		batch := currentQueue[i:end]

		for _, path := range batch {
			if err := os.Chtimes(path, now, now); err == nil {
				count++
			}
		}
	}

	if count > 0 {
		slog.Info("Touched files", "count", count)
	}
}
