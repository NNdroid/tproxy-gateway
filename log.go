package main

import (
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

type LogRingBuffer struct {
	mu      sync.RWMutex
	entries []LogEntry
	maxSize int
}

var (
	GlobalLogBuffer *LogRingBuffer
	atomicLogLevel  zap.AtomicLevel
)

func init() {
	GlobalLogBuffer = &LogRingBuffer{
		entries: make([]LogEntry, 0, 500),
		maxSize: 500,
	}
	atomicLogLevel = zap.NewAtomicLevelAt(zap.InfoLevel)
}

func (b *LogRingBuffer) Push(level, msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	entry := LogEntry{
		Timestamp: time.Now().Format("15:04:05.000"),
		Level:     level,
		Message:   msg,
	}
	if len(b.entries) >= b.maxSize {
		b.entries = b.entries[1:]
	}
	b.entries = append(b.entries, entry)
}

func (b *LogRingBuffer) GetEntries(limit int) []LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if limit <= 0 || limit > len(b.entries) {
		limit = len(b.entries)
	}

	result := make([]LogEntry, limit)
	copy(result, b.entries[len(b.entries)-limit:])
	return result
}

type ringBufferSyncer struct{}

func (s *ringBufferSyncer) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		level := "INFO"
		if strings.Contains(msg, "DEBUG") {
			level = "DEBUG"
		} else if strings.Contains(msg, "WARN") {
			level = "WARN"
		} else if strings.Contains(msg, "ERROR") {
			level = "ERROR"
		}
		GlobalLogBuffer.Push(level, msg)
	}
	return len(p), nil
}

func SetLogLevel(levelStr string) string {
	var level zapcore.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = zap.DebugLevel
	case "info":
		level = zap.InfoLevel
	case "warn":
		level = zap.WarnLevel
	case "error":
		level = zap.ErrorLevel
	default:
		level = zap.InfoLevel
	}
	atomicLogLevel.SetLevel(level)
	if cfg != nil {
		cfg.Log.Level = strings.ToLower(levelStr)
	}
	return strings.ToUpper(levelStr)
}

func InitLogger(levelStr string) {
	SetLogLevel(levelStr)

	encoderConfig := zap.NewDevelopmentEncoderConfig()
	encoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder

	stdoutSyncer := zapcore.AddSync(os.Stdout)
	ringSyncer := zapcore.AddSync(&ringBufferSyncer{})

	multiSyncer := zapcore.NewMultiWriteSyncer(stdoutSyncer, ringSyncer)

	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderConfig),
		multiSyncer,
		atomicLogLevel,
	)

	logger := zap.New(core, zap.AddCaller())
	zap.ReplaceGlobals(logger)
}
