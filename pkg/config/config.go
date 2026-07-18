package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

var Cfg *Config

type Config struct {
	// [common]
	RuntimePath string
	DataPath    string
	LogPath     string

	// [wiki]
	DefaultScanIntervalSec     int
	ScanOnlyScanIntervalSec    int
	WikiWriteDebounceSec       int
	EventDebounceMs            int
	ChildMapAggregateThreshold int
	RecentChangesKeep          int
	RecentChangesRetentionDays int
	ShutdownFlushTimeoutSec    int
	ContainerDirs              []string

	// [throttled indexing] spec §4.1-4.6: fuses/caps/throttles bounding
	// file_events growth on huge roots.
	EventFuseHigh       int
	EventFuseLow        int
	GlobalFuseHigh      int
	GlobalFuseLow       int
	EventMaxRows        int64
	WalkThrottleEvery   int
	WalkThrottleSleepMs int
	PrecheckDirLimit    int
	PrecheckTimeoutMs   int
	ReconcileBatchSize  int
}

func Init(configFile, confSample string) error {
	if configFile == "" {
		configFile = "/etc/nimoos/wiki.conf"
	}
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		if err := os.WriteFile(configFile, []byte(confSample), 0644); err != nil {
			return fmt.Errorf("failed to write default config: %w", err)
		}
	}

	v := viper.New()
	v.SetConfigFile(configFile)
	v.SetConfigType("ini")
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	Cfg = &Config{
		RuntimePath:                v.GetString("common.RuntimePath"),
		DataPath:                   v.GetString("common.DataPath"),
		LogPath:                    v.GetString("common.LogPath"),
		DefaultScanIntervalSec:     v.GetInt("wiki.DefaultScanIntervalSec"),
		ScanOnlyScanIntervalSec:    v.GetInt("wiki.ScanOnlyScanIntervalSec"),
		WikiWriteDebounceSec:       v.GetInt("wiki.WikiWriteDebounceSec"),
		EventDebounceMs:            v.GetInt("wiki.EventDebounceMs"),
		ChildMapAggregateThreshold: v.GetInt("wiki.ChildMapAggregateThreshold"),
		RecentChangesKeep:          v.GetInt("wiki.RecentChangesKeep"),
		RecentChangesRetentionDays: v.GetInt("wiki.RecentChangesRetentionDays"),
		ShutdownFlushTimeoutSec:    v.GetInt("wiki.ShutdownFlushTimeoutSec"),
		EventFuseHigh:              v.GetInt("wiki.EventFuseHigh"),
		EventFuseLow:               v.GetInt("wiki.EventFuseLow"),
		GlobalFuseHigh:             v.GetInt("wiki.GlobalFuseHigh"),
		GlobalFuseLow:              v.GetInt("wiki.GlobalFuseLow"),
		EventMaxRows:               v.GetInt64("wiki.EventMaxRows"),
		WalkThrottleEvery:          v.GetInt("wiki.WalkThrottleEvery"),
		WalkThrottleSleepMs:        v.GetInt("wiki.WalkThrottleSleepMs"),
		PrecheckDirLimit:           v.GetInt("wiki.PrecheckDirLimit"),
		PrecheckTimeoutMs:          v.GetInt("wiki.PrecheckTimeoutMs"),
		ReconcileBatchSize:         v.GetInt("wiki.ReconcileBatchSize"),
	}
	if raw := v.GetString("wiki.ContainerDirs"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				Cfg.ContainerDirs = append(Cfg.ContainerDirs, s)
			}
		}
	}

	applyDefaults(Cfg)
	return nil
}

func applyDefaults(c *Config) {
	if c.RuntimePath == "" {
		c.RuntimePath = "/var/run/nimoos"
	}
	if c.DataPath == "" {
		c.DataPath = "/var/lib/nimoos/wiki"
	}
	if c.LogPath == "" {
		c.LogPath = "/var/log/nimoos"
	}
	if c.DefaultScanIntervalSec == 0 {
		c.DefaultScanIntervalSec = 21600
	}
	if c.ScanOnlyScanIntervalSec == 0 {
		c.ScanOnlyScanIntervalSec = 600
	}
	if c.WikiWriteDebounceSec == 0 {
		c.WikiWriteDebounceSec = 5
	}
	if c.EventDebounceMs == 0 {
		c.EventDebounceMs = 200
	}
	if c.ChildMapAggregateThreshold == 0 {
		c.ChildMapAggregateThreshold = 50
	}
	if c.RecentChangesKeep == 0 {
		c.RecentChangesKeep = 20
	}
	if c.RecentChangesRetentionDays == 0 {
		c.RecentChangesRetentionDays = 90
	}
	if c.ShutdownFlushTimeoutSec == 0 {
		c.ShutdownFlushTimeoutSec = 5
	}
	if c.EventFuseHigh == 0 {
		c.EventFuseHigh = 50000
	}
	if c.EventFuseLow == 0 {
		c.EventFuseLow = 5000
	}
	if c.GlobalFuseHigh == 0 {
		c.GlobalFuseHigh = 200000
	}
	if c.GlobalFuseLow == 0 {
		c.GlobalFuseLow = 20000
	}
	if c.EventMaxRows == 0 {
		c.EventMaxRows = 1000000
	}
	if c.WalkThrottleEvery == 0 {
		c.WalkThrottleEvery = 1000
	}
	if c.WalkThrottleSleepMs == 0 {
		c.WalkThrottleSleepMs = 50
	}
	if c.PrecheckDirLimit == 0 {
		c.PrecheckDirLimit = 20000
	}
	if c.PrecheckTimeoutMs == 0 {
		c.PrecheckTimeoutMs = 2000
	}
	if c.ReconcileBatchSize == 0 {
		c.ReconcileBatchSize = 5000
	}
	if len(c.ContainerDirs) == 0 {
		c.ContainerDirs = []string{
			"node_modules", ".git", "__pycache__", "venv", ".venv", ".tox",
			"target", "build", "dist", ".next", ".nuxt", "vendor",
			".gradle", ".cargo", ".cache", ".idea", ".vscode",
		}
	}
}
