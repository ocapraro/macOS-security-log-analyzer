package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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

	ollamaEndpoint, llmInstance, llmConfigErr := loadLLMConfig(repoRoot)
	if llmConfigErr != nil {
		fmt.Fprintf(os.Stderr, "warning: LLM log analysis disabled: %v\n", llmConfigErr)
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
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to access monitor stderr: %v\n", err)
		os.Exit(1)
	}

	if _, err := fmt.Fprintf(logFile, "\n[RUN_START] %s monitor=%s\n", time.Now().Format(time.RFC3339), monitorPath); err != nil {
		fmt.Fprintf(os.Stderr, "unable to write run start marker: %v\n", err)
		os.Exit(1)
	}

	var logFileMu sync.Mutex
	var logLines chan string
	var llmDone chan struct{}
	if llmConfigErr == nil {
		logLines = make(chan string, 4096)
		llmDone = make(chan struct{})
		go func() {
			runLLMAnalysisLoop(logLines, logFile, &logFileMu, ollamaEndpoint, llmInstance)
			close(llmDone)
		}()
	}

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

	streamErrors := make(chan error, 2)
	go func() {
		streamErrors <- streamAndFormat(stdout, logFile, &logFileMu, logLines)
	}()
	go func() {
		streamErrors <- streamAndFormat(stderr, logFile, &logFileMu, logLines)
	}()

	for i := 0; i < 2; i++ {
		if err := <-streamErrors; err != nil {
			fmt.Fprintf(os.Stderr, "error while reading monitor output: %v\n", err)
		}
	}
	if logLines != nil {
		close(logLines)
	}
	if llmDone != nil {
		<-llmDone
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

func streamAndFormat(reader io.Reader, logFile *os.File, logFileMu *sync.Mutex, logLines chan<- string) error {
	scanner := bufio.NewScanner(reader)
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		formatted := formatLine(line)
		if err := appendLogLine(logFile, logFileMu, formatted); err != nil {
			return err
		}
		if logLines != nil {
			logLines <- formatted
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

func loadLLMConfig(repoRoot string) (string, string, error) {
	envPath := filepath.Join(repoRoot, ".env")
	values, err := parseDotEnv(envPath)
	if err != nil {
		return "", "", err
	}

	endpoint := firstNonEmpty(values["OLLAMA_ENDPOINT"], os.Getenv("OLLAMA_ENDPOINT"))
	model := firstNonEmpty(values["LLM_INSTANCE"], os.Getenv("LLM_INSTANCE"))

	if strings.TrimSpace(endpoint) == "" {
		return "", "", errors.New("OLLAMA_ENDPOINT is required in .env")
	}
	if strings.TrimSpace(model) == "" {
		return "", "", errors.New("LLM_INSTANCE is required in .env")
	}

	return strings.TrimRight(endpoint, "/"), model, nil
}

func parseDotEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("unable to open .env file at %s: %w", path, err)
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, `"'`)

		if key != "" {
			values[key] = value
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("unable to parse .env file: %w", err)
	}

	return values, nil
}

type llmLogAssessment struct {
	Reasoning  string `json:"reasoning"`
	Assessment string `json:"assessment"`
	Log        string `json:"log"`
}

func runLLMAnalysisLoop(logLines <-chan string, logFile *os.File, logFileMu *sync.Mutex, ollamaEndpoint, llmInstance string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	pending := make([]string, 0, 8)
	var oldest time.Time

	flush := func(trigger string) {
		if len(pending) == 0 {
			return
		}

		batch := append([]string(nil), pending...)
		pending = pending[:0]
		oldest = time.Time{}

		fmt.Printf("LLM dispatch (%s): sending %d new logs\n", trigger, len(batch))

		batches := [][]string{batch}
		if len(batch) > 5 {
			mid := (len(batch) + 1) / 2
			batches = [][]string{batch[:mid], batch[mid:]}
			fmt.Printf("LLM dispatch: splitting into %d concurrent requests (%d + %d logs)\n", len(batches), len(batches[0]), len(batches[1]))
		}

		type result struct {
			items []llmLogAssessment
			err   error
		}

		results := make(chan result, len(batches))
		for i := range batches {
			go func(logChunk []string) {
				items, err := analyzeLogBatch(logChunk, ollamaEndpoint, llmInstance)
				results <- result{items: items, err: err}
			}(batches[i])
		}

		allItems := make([]llmLogAssessment, 0, len(batch))
		for i := 0; i < len(batches); i++ {
			result := <-results
			if result.err != nil {
				fmt.Fprintf(os.Stderr, "LLM dispatch failed: %v\n", result.err)
				continue
			}
			allItems = append(allItems, result.items...)
		}

		if len(allItems) == 0 {
			fmt.Println("LLM dispatch returned no parsed assessments.")
			return
		}

		payload, err := json.MarshalIndent(allItems, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to serialize LLM analysis: %v\n", err)
			return
		}

		fmt.Printf("LLM response received for %d logs\n", len(allItems))
		fmt.Println(string(payload))

		if err := appendLogBlock(logFile, logFileMu, fmt.Sprintf("[LLM_ANALYSIS_JSON]\n%s", string(payload))); err != nil {
			fmt.Fprintf(os.Stderr, "failed to append LLM analysis to log: %v\n", err)
		}
	}

	for {
		select {
		case line, ok := <-logLines:
			if !ok {
				flush("shutdown")
				return
			}

			if len(pending) == 0 {
				oldest = time.Now()
			}
			pending = append(pending, line)

			if len(pending) >= 5 {
				flush("count>=5")
			}
		case <-ticker.C:
			if len(pending) > 0 && !oldest.IsZero() && time.Since(oldest) >= 1*time.Minute {
				flush("age>=1m")
			}
		}
	}
}

func analyzeLogBatch(logLines []string, ollamaEndpoint, llmInstance string) ([]llmLogAssessment, error) {
	logsJSON, err := json.Marshal(logLines)
	if err != nil {
		return nil, fmt.Errorf("unable to prepare log batch: %w", err)
	}

	prompt := fmt.Sprintf(
		"You are a security log analyst. For EACH log in the JSON array below, return a JSON array with exactly one object per log in the same order. Each object must contain: reasoning (string), assessment (one of normal/suspicious/dangerous), and log (the exact original log line). Return ONLY raw JSON.\\n\\nLogs: %s",
		string(logsJSON),
	)

	payload := map[string]any{
		"model":  llmInstance,
		"prompt": prompt,
		"stream": false,
	}

	requestBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("unable to build Ollama request: %w", err)
	}

	requestURL := ollamaGenerateURL(ollamaEndpoint)
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Post(requestURL, "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("unable to call Ollama endpoint %s: %w", requestURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read Ollama response: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("Ollama API returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Response string `json:"response"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unable to parse Ollama response JSON: %w", err)
	}

	if strings.TrimSpace(parsed.Error) != "" {
		return nil, errors.New(parsed.Error)
	}

	jsonPayload, err := extractJSONArrayPayload(parsed.Response)
	if err != nil {
		return nil, err
	}

	var assessments []llmLogAssessment
	if err := json.Unmarshal([]byte(jsonPayload), &assessments); err != nil {
		return nil, fmt.Errorf("unable to parse LLM assessment array: %w", err)
	}

	for i := range assessments {
		assessments[i].Assessment = strings.ToLower(strings.TrimSpace(assessments[i].Assessment))
		if assessments[i].Assessment != "normal" && assessments[i].Assessment != "suspicious" && assessments[i].Assessment != "dangerous" {
			assessments[i].Assessment = "suspicious"
		}
		if strings.TrimSpace(assessments[i].Log) == "" && i < len(logLines) {
			assessments[i].Log = logLines[i]
		}
	}

	if len(assessments) == 0 {
		return nil, errors.New("LLM returned empty assessment list")
	}

	return assessments, nil
}

func extractJSONArrayPayload(response string) (string, error) {
	trimmed := strings.TrimSpace(response)
	if trimmed == "" {
		return "", errors.New("LLM returned empty response")
	}

	start := strings.Index(trimmed, "[")
	end := strings.LastIndex(trimmed, "]")
	if start == -1 || end == -1 || end < start {
		return "", fmt.Errorf("LLM response did not include a JSON array: %s", trimmed)
	}

	return trimmed[start : end+1], nil
}

func appendLogLine(logFile *os.File, logFileMu *sync.Mutex, line string) error {
	logFileMu.Lock()
	defer logFileMu.Unlock()
	_, err := fmt.Fprintln(logFile, line)
	return err
}

func appendLogBlock(logFile *os.File, logFileMu *sync.Mutex, block string) error {
	logFileMu.Lock()
	defer logFileMu.Unlock()
	_, err := fmt.Fprintf(logFile, "\n%s\n", block)
	return err
}

func ollamaGenerateURL(endpoint string) string {
	normalized := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if strings.HasSuffix(normalized, "/api/generate") {
		return normalized
	}
	return normalized + "/api/generate"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
