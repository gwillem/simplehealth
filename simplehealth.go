package simplehealth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	maxLoad          = 0.8
	maxOpenFilesPerc = 0.9
	maxDiskPerc      = 0.9
)

type SimpleHealth struct {
	checks []func() error
}

var defaultChecks = []func() error{
	checkOpenFiles,
	checkDisk,
	checkLoad,
}

func NewSimpleHealth() *SimpleHealth {
	return &SimpleHealth{checks: defaultChecks}
}

func (s *SimpleHealth) AddCheck(check func() error) {
	s.checks = append(s.checks, check)
}

func (s *SimpleHealth) Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	errs := s.Check()
	if len(errs) > 0 {
		w.WriteHeader(http.StatusInternalServerError)
		errorMessages := make([]string, len(errs))
		for i, err := range errs {
			errorMessages[i] = err.Error()
		}
		data := map[string]any{
			"status": "MUCHSAD",
			"errors": errorMessages,
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(data)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "VERYHAPPY",
	})
}

func (s *SimpleHealth) Check() []error {
	var errs []error
	errCh := make(chan error, len(s.checks))

	for _, check := range s.checks {
		go func(c func() error) {
			errCh <- c()
		}(check)
	}

	for range s.checks {
		if err := <-errCh; err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

func checkLoad() error {
	avg, err := load.Avg()
	if err != nil {
		return err
	}
	numCPU := runtime.NumCPU()
	if got := avg.Load5 / float64(numCPU); got > maxLoad {
		return fmt.Errorf("high load5 per cpu: %f", got)
	}
	return nil
}

func checkOpenFiles() error {
	processes, err := process.Processes()
	if err != nil {
		return err
	}

	for _, p := range processes {
		user, _ := p.Username()
		name, _ := p.Name()
		pname := fmt.Sprintf("%d/%s/%s", p.Pid, user, name)

		rlimits, err := p.Rlimit()
		if err != nil {
			continue
		}

		softLimit := rlimits[syscall.RLIMIT_NOFILE].Soft
		if softLimit <= 0 {
			// Skip processes with no file limits
			continue
		}

		cur, err := p.NumFDs()
		if err != nil || cur == 0 {
			continue
		}

		usage := float64(cur) / float64(softLimit)
		if usage > maxOpenFilesPerc {
			return fmt.Errorf("%s uses %d%% open files, are we growing too fast?", pname, int(usage*100))
		}
	}
	return nil
}

func checkDisk() error {
	parts, err := disk.Partitions(false)
	if err != nil {
		return err
	}

	for _, part := range parts {
		if strings.Contains(part.Device, "loop") || strings.Contains(part.Mountpoint, "/snap/") ||
			strings.Contains(part.Mountpoint, "/boot") ||
			strings.Contains(part.Device, "devfs") {
			continue
		}

		usage, err := disk.Usage(part.Mountpoint)
		if err != nil {
			continue
		}

		// log.Printf("Disk %s bytes is %.0f%% full\n", part.Mountpoint, usage.UsedPercent)
		if usage.UsedPercent >= 100*maxDiskPerc {
			return fmt.Errorf("disk %s bytes %.0f%% full", part.Mountpoint, usage.UsedPercent)
		}

		statvfs := syscall.Statfs_t{}
		err = syscall.Statfs(part.Mountpoint, &statvfs)
		if err != nil {
			continue
		}
		if statvfs.Files > 0 {
			percInodes := 100.0 * float64(statvfs.Files-statvfs.Ffree) / float64(statvfs.Files)
			// log.Printf("Disk %s inodes is %.0f%% full\n", part.Mountpoint, percInodes)
			if percInodes >= 100*maxDiskPerc {
				return fmt.Errorf("disk %s inodes %.0f%% full", part.Mountpoint, percInodes)
			}
		}
	}
	return nil
}

func AgeOfNewestFile(glob string) (float64, error) {
	files, err := filepath.Glob(glob)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no files found at %s", glob)
	}

	var newest time.Time
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			return 0, err
		}

		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}

	return time.Since(newest).Hours() / 24, nil
}
