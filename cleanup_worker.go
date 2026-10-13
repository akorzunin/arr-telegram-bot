package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func validCleanupTask(task CleanupTask) bool {
	if len(task.ID) != 16 {
		return false
	}
	for _, c := range task.ID {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	if task.Scope.ItemID <= 0 {
		return false
	}
	parts := strings.Split(task.Output, string(filepath.Separator))
	if len(parts) < 2 {
		return false
	}
	switch task.Scope.Kind {
	case "movie":
		return parts[0] == "films"
	case "series":
		if task.Scope.Season < -1 || parts[0] != "shows" {
			return false
		}
		return task.Scope.Season < 0 || (len(parts) >= 3 && parts[len(parts)-1] == fmt.Sprintf("Season %02d", task.Scope.Season))
	}
	return false
}

// Walk existing ancestors without following symlinks. The output tree is
// worker-owned; external concurrent renames would require dirfd/openat deletion.
func cleanupDestination(root, relative string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("invalid output root")
	}
	path, err := confined(root, relative)
	if err != nil {
		return "", err
	}
	current := root
	parts := append([]string{""}, strings.Split(relative, string(filepath.Separator))...)
	for _, part := range parts {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if part == "" {
				return "", errors.New("output mount is missing")
			}
			break
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return "", errors.New("unsafe cleanup directory")
		}
	}
	return path, nil
}

func (w *Worker) cleanupTick() error {
	entries, err := os.ReadDir(filepath.Join(w.Queue, "cleanup"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var task CleanupTask
		if err := readJSON(filepath.Join(w.Queue, "cleanup", entry.Name()), &task); err != nil {
			return err
		}
		if !validCleanupTask(task) || entry.Name() != task.ID+".json" {
			return errors.New("invalid cleanup task")
		}
		done := filepath.Join(w.Queue, "cleanup-done", task.ID)
		if _, err := os.Stat(done); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		destination, err := cleanupDestination(w.Output, task.Output)
		if err != nil {
			return err
		}
		// tick runs on the single worker thread, never alongside an encode or
		// subtitle sync. Durable cancellation prevents deleted outputs resurrecting.
		if err := cancelCleanupJobs(w.Queue, task.Output); err != nil {
			return err
		}
		jobs, err := os.ReadDir(filepath.Join(w.Queue, "requests"))
		if err != nil {
			return err
		}
		for _, entry := range jobs {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			var job PrepareJob
			if err := readJSON(filepath.Join(w.Queue, "requests", entry.Name()), &job); err != nil {
				return err
			}
			if !outputInScope(job.Output, task.Output) {
				continue
			}
			stage, err := cleanupDestination(w.Output, filepath.Join(".staging", job.ID))
			if err != nil {
				return err
			}
			if err := os.RemoveAll(stage); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(destination); err != nil {
			return err
		}
		if err := w.jf(context.Background(), "POST", "/Library/Refresh", nil); err != nil {
			return err
		}
		if err := atomicWrite(done, []byte("complete")); err != nil {
			return err
		}
	}
	return nil
}
