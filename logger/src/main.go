package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type event struct {
	TS          int64  `json:"ts"`
	Event       string `json:"event"`
	PID         int    `json:"pid"`
	PPID        int    `json:"ppid"`
	ChildPID    int    `json:"child_pid"`
	ProcessPath string `json:"process_path"`
	TargetPath  string `json:"target_path"`
}

func main() {
	repoRoot, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to locate repository root: %v\n", err)
		os.Exit(1)
	}

	logFilePath := filepath.Join(repoRoot, "logger", "out", "verbose.log")
	if err := os.MkdirAll(filepath.Dir(logFilePath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "unable to create log directory: %v\n", err)
		os.Exit(1)
	}

	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to open verbose log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	monitorPath, err := resolveOrBuildMonitor(repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to resolve monitor executable: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Logger started. Running monitor: %s\n", monitorPath)
	fmt.Printf("Verbose logs will be written to: %s\n", logFilePath)

	cmd := exec.Command(monitorPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to access monitor stdout: %v\n", err)
		os.Exit(1)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start monitor: %v\n", err)
		os.Exit(1)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop
		fmt.Println("\nReceived stop signal. Shutting down monitor...")
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}()

	if err := streamAndFormat(stdout, logFile); err != nil {
		fmt.Fprintf(os.Stderr, "error while reading monitor output: %v\n", err)
	}

	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "monitor terminated with error: %v\n", err)
		}
	}
}

func findRepoRoot() (string, error) {
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		candidate := filepath.Join(current, "monitor", "src", "main.c")
		if _, err := os.Stat(candidate); err == nil {
			return current, nil
		}

		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	return "", errors.New("monitor/src/main.c not found in any parent directory")
}

func resolveOrBuildMonitor(repoRoot string) (string, error) {
	candidates := []string{
		filepath.Join(repoRoot, "es-test"),
		filepath.Join(repoRoot, "out", "es-test"),
		filepath.Join(repoRoot, "monitor", "out", "es-test"),
	}

	for _, candidate := range candidates {
		if isExecutable(candidate) {
			return candidate, nil
		}
	}

	outputPath := filepath.Join(repoRoot, "monitor", "out", "es-test")
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return "", err
	}

	source := filepath.Join(repoRoot, "monitor", "src", "main.c")
	build := exec.Command("clang", "-fcolor-diagnostics", "-fansi-escape-codes", "-g", source, "-o", outputPath)
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr

	fmt.Println("No monitor executable found. Building monitor with clang...")
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("failed to compile monitor: %w", err)
	}

	if !isExecutable(outputPath) {
		return "", errors.New("monitor build completed but executable is missing")
	}

	return outputPath, nil
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

func streamAndFormat(reader io.Reader, logFile *os.File) error {
	writer := io.MultiWriter(os.Stdout, logFile)
	scanner := bufio.NewScanner(reader)
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		formatted := formatLine(line)
		if _, err := fmt.Fprintln(writer, formatted); err != nil {
			return err
		}
	}

	return scanner.Err()
}

func formatLine(line string) string {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return fmt.Sprintf("[monitor] %s", line)
	}

	timeStamp := time.Unix(e.TS, 0).Format(time.RFC3339)

	switch e.Event {
	case "exec":
		return fmt.Sprintf(
			"[%s] EXEC: process pid=%d ppid=%d launched %s and targeted %s",
			timeStamp,
			e.PID,
			e.PPID,
			emptyFallback(e.ProcessPath, "unknown process"),
			emptyFallback(e.TargetPath, "unknown target"),
		)
	case "fork":
		return fmt.Sprintf(
			"[%s] FORK: parent pid=%d created child pid=%d",
			timeStamp,
			e.PID,
			e.ChildPID,
		)
	case "write":
		return fmt.Sprintf(
			"[%s] WRITE: process pid=%d wrote to %s",
			timeStamp,
			e.PID,
			emptyFallback(e.TargetPath, "unknown path"),
		)
	default:
		return fmt.Sprintf("[%s] EVENT(%s): raw=%s", timeStamp, emptyFallback(e.Event, "unknown"), line)
	}
}

func emptyFallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
