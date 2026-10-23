// Package traffic records per-port byte counts without exposing a network listener.
package traffic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

type Usage struct {
	dataStore    sync.Map
	shutdownCtx  context.Context
	logger       *logrus.Logger
	sniffer      bool
	snifferLog   string
	totalTraffic atomic.Uint64
	fileMu       sync.Mutex
}
type PortUsage struct {
	Port  int
	Usage uint64
}

func NewDataStore(ctx context.Context, log string, enabled bool, logger *logrus.Logger) *Usage {
	return &Usage{shutdownCtx: ctx, logger: logger, sniffer: enabled, snifferLog: log}
}

// Run flushes usage on a fixed cadence and on shutdown. It never opens a socket.
func (m *Usage) Run() {
	if !m.sniffer {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	defer m.saveUsageData()
	for {
		select {
		case <-m.shutdownCtx.Done():
			return
		case <-ticker.C:
			m.saveUsageData()
		}
	}
}
func (m *Usage) AddOrUpdatePort(port int, usage uint64) {
	value, ok := m.dataStore.Load(port)
	if !ok {
		value, _ = m.dataStore.LoadOrStore(port, &atomic.Uint64{})
	}
	value.(*atomic.Uint64).Add(usage)
}

func (m *Usage) saveUsageData() {
	// The whole read-merge-write is one operation on the file. It runs on a
	// timer in its own goroutine, and a save that takes longer than the tick
	// used to be joined by the next one: two of them reading the same file,
	// merging against it and writing back, with whichever finished last
	// silently discarding the other's totals.
	m.fileMu.Lock()
	defer m.fileMu.Unlock()

	// Step 1: Load existing usage data from the JSON file
	var existingUsageData []PortUsage
	file, err := os.Open(m.snifferLog)
	if err == nil {
		// If the file exists, decode the JSON data into existingUsageData
		err = json.NewDecoder(file).Decode(&existingUsageData)
		_ = file.Close()
		if err != nil {
			m.logger.Errorf("error decoding JSON data: %v", err)
			return
		}
	} else if !os.IsNotExist(err) {
		// Log any error except file not existing
		m.logger.Errorf("error opening JSON file: %v", err)
		return
	}

	// Step 2: Get current usage data from sync.Map
	currentUsageData := m.collectUsageDataFromSyncMap()
	committed := false
	defer func() {
		if !committed {
			// Swap separated this snapshot from concurrent additions. Put it
			// back on any failure so a later save can retry every byte.
			for _, usage := range currentUsageData {
				m.AddOrUpdatePort(usage.Port, usage.Usage)
			}
		}
	}()

	// Step 3: Merge the existing and current usage data into a map to avoid duplicates
	usageMap := make(map[int]PortUsage)

	// Add existing usage data to the map
	for _, usage := range existingUsageData {
		usageMap[usage.Port] = usage
	}

	// Append or update current usage data in the map
	for _, usage := range currentUsageData {
		if existing, exists := usageMap[usage.Port]; exists {
			// Update existing port usage
			existing.Usage += usage.Usage
			usageMap[usage.Port] = existing
		} else {
			// Add new port usage
			usageMap[usage.Port] = usage
		}
	}

	// Step 4: Convert the map back to a slice
	var mergedUsageData []PortUsage
	var total uint64
	for _, usage := range usageMap {
		mergedUsageData = append(mergedUsageData, usage)
		total += usage.Usage
	}

	// Step 5: Convert merged data to JSON
	data, err := json.MarshalIndent(mergedUsageData, "", "  ")
	if err != nil {
		m.logger.Errorf("error marshalling usage data: %v", err)
		return
	}

	// Step 6: Write JSON data to file
	err = m.writeUsageData(data)
	if err != nil {
		m.logger.Errorf("error writing usage data to file: %v", err)
		return
	}
	committed = true
	// Published once, at the end. Zeroing the counter and adding the ports
	// back one at a time meant the stats endpoint could read a total that was
	// part-way through being rebuilt — and, on the tick that reset it, read
	// zero and report the tunnel as having carried nothing.
	m.totalTraffic.Store(total)
}

func (m *Usage) getUsageFromFile() []PortUsage {
	// Reading shares the lock with saving, so a page load cannot catch the
	// file part-way through being rewritten.
	m.fileMu.Lock()
	defer m.fileMu.Unlock()

	// Check if the file exists
	if _, err := os.Stat(m.snifferLog); os.IsNotExist(err) {
		// If the file does not exist, create it and write "null"
		file, err := os.OpenFile(m.snifferLog, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			m.logger.Errorf("error creating file: %v", err)
			return nil
		}
		// Closed on the way out however this branch ends; the success path
		// used to return without closing it at all.
		defer file.Close()

		// Write "null" to the new file
		if _, err := file.Write([]byte("null")); err != nil {
			m.logger.Errorf("error writing 'null' to the file: %v", err)
			return nil
		}

		return nil
	}

	var usageData []PortUsage

	// Open the JSON file
	file, err := os.Open(m.snifferLog)
	if err != nil {
		m.logger.Errorf("error opening JSON file: %v", err)
		return nil
	}
	defer file.Close()

	// Decode the JSON file into the usageData slice
	err = json.NewDecoder(file).Decode(&usageData)
	if err != nil {
		m.logger.Errorf("error decoding JSON data: %v", err)
		return nil
	}

	// Sort usageData by Port in ascending order
	sort.Slice(usageData, func(i, j int) bool {
		return usageData[i].Port < usageData[j].Port
	})

	return usageData
}

// collectUsageDataFromSyncMap atomically takes each port's pending usage.
// A writer may have loaded the counter before this pass, so keep its entry and
// reset only the value. Deleting it would orphan that writer's next addition.
func (m *Usage) collectUsageDataFromSyncMap() []PortUsage {
	var usageData []PortUsage
	m.dataStore.Range(func(key, value interface{}) bool {
		if usage := value.(*atomic.Uint64).Swap(0); usage > 0 {
			usageData = append(usageData, PortUsage{Port: key.(int), Usage: usage})
		}
		return true
	})
	return usageData
}

// writeUsageData replaces the log only once a complete new document is ready.
// A failed write must leave the previous committed totals readable for retry.
func (m *Usage) writeUsageData(data []byte) error {
	path := m.snifferLog
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		// The old writer followed symlinks. Replace the resolved target,
		// retaining the configured link and writing in the target directory.
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// A dangling link must not be silently replaced with a regular log.
		// Keep the pending counters until its destination becomes available.
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".usage-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	defer file.Close()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
