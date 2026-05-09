package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	ksmPagesToScan = "1000"
	ksmSleepMillis = "1000"
	defaultKSMRoot = "/sys/kernel/mm/ksm"
	ksmRunValue    = "1"
)

var ksmRootPath = defaultKSMRoot

func (o *LinuxOps) EnsureKSM(ctx context.Context) error {
	if _, err := os.Stat(ksmRootPath); err != nil {
		if os.IsNotExist(err) {
			o.logger.WarnContext(ctx, "KSM sysfs not available; continuing without host memory deduplication", "path", ksmRootPath)
			return nil
		}
		return fmt.Errorf("stat KSM sysfs: %w", err)
	}
	run, err := readKSMValue("run")
	if err != nil {
		return fmt.Errorf("read KSM run state: %w", err)
	}
	if run != ksmRunValue {
		if err := writeKSMValueIfPresent("pages_to_scan", ksmPagesToScan); err != nil {
			return err
		}
		if err := writeKSMValueIfPresent("sleep_millisecs", ksmSleepMillis); err != nil {
			return err
		}
		if err := writeKSMValue("run", ksmRunValue); err != nil {
			return fmt.Errorf("enable KSM: %w", err)
		}
	}
	o.logger.InfoContext(ctx, "KSM host memory deduplication enabled",
		"run", readKSMValueOrUnknown("run"),
		"pages_to_scan", readKSMValueOrUnknown("pages_to_scan"),
		"sleep_millisecs", readKSMValueOrUnknown("sleep_millisecs"),
		"pages_sharing", readKSMValueOrUnknown("pages_sharing"),
	)
	return nil
}

func readKSMValue(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(ksmRootPath, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func readKSMValueOrUnknown(name string) string {
	value, err := readKSMValue(name)
	if err != nil {
		return "unknown"
	}
	return value
}

func writeKSMValueIfPresent(name string, value string) error {
	err := writeKSMValue(name, value)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func writeKSMValue(name string, value string) error {
	return os.WriteFile(filepath.Join(ksmRootPath, name), []byte(value+"\n"), 0o644)
}
