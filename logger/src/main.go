package main

import (
	"bufio"
	"bytes"
	"context"
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

const (
	llmBatchThreshold = 25
	llmRetryBackoff   = 15 * time.Second
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
	llmCtx, llmCancel := context.WithCancel(context.Background())
	defer llmCancel()

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
	analysisFilePath := filepath.Join(repoRoot, "logger", "out", "analysis.jsonl")

	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to open verbose log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	var analysisFile *os.File
	if llmConfigErr == nil {
		analysisFile, err = os.OpenFile(analysisFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to open analysis log file: %v\n", err)
			os.Exit(1)
		}
		defer analysisFile.Close()
	}

	monitorPath, err := resolveOrBuildMonitor(repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to resolve monitor executable: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Logger started. Running monitor: %s\n", monitorPath)
	fmt.Printf("Verbose logs will be written to: %s\n", logFilePath)
	if llmConfigErr == nil {
		fmt.Printf("LLM analysis will be written to: %s\n", analysisFilePath)
	}

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
	var analysisFileMu sync.Mutex
	var logLines chan string
	var llmDone chan struct{}
	if llmConfigErr == nil {
		logLines = make(chan string, 4096)
		llmDone = make(chan struct{})
		go func() {
			runLLMAnalysisLoop(llmCtx, logLines, analysisFile, &analysisFileMu, ollamaEndpoint, llmInstance)
			close(llmDone)
		}()
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start monitor: %v\n", err)
		os.Exit(1)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	go func() {
		sig := <-stop
		fmt.Printf("\nReceived stop signal (%s). Attempting graceful shutdown...\n", sig.String())
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}

		select {
		case second := <-stop:
			fmt.Printf("Received second stop signal (%s). Forcing monitor shutdown now...\n", second.String())
			fmt.Println("Cancelling in-flight LLM requests...")
			llmCancel()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		case <-time.After(5 * time.Second):
			fmt.Println("Graceful shutdown timed out. Forcing monitor shutdown...")
			fmt.Println("Cancelling in-flight LLM requests...")
			llmCancel()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
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
		select {
		case <-llmDone:
		case <-time.After(2 * time.Second):
			fmt.Println("LLM worker is still shutting down; exiting without waiting further.")
		}
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

type llmBatchAssessment struct {
	Timestamp     string   `json:"timestamp"`
	Reasoning     string   `json:"reasoning"`
	NormalLogs    []string `json:"normalLogs"`
	SuspicousLogs []string `json:"suspicousLogs"`
	DangerousLogs []string `json:"dangerousLogs"`
}

func runLLMAnalysisLoop(ctx context.Context, logLines <-chan string, analysisFile *os.File, analysisFileMu *sync.Mutex, ollamaEndpoint, llmInstance string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	pending := make([]string, 0, 32)
	var oldest time.Time
	var retryAfter time.Time

	flush := func(trigger string, force bool) {
		if len(pending) == 0 {
			return
		}
		if !force && !retryAfter.IsZero() && time.Now().Before(retryAfter) {
			return
		}

		batch := append([]string(nil), pending...)
		pending = pending[:0]
		oldest = time.Time{}

		fmt.Printf("LLM dispatch (%s): sending %d new logs\n", trigger, len(batch))

		batches := [][]string{batch}
		if len(batch) > 25 {
			mid := (len(batch) + 1) / 2
			batches = [][]string{batch[:mid], batch[mid:]}
			fmt.Printf("LLM dispatch: splitting into %d concurrent requests (%d + %d logs)\n", len(batches), len(batches[0]), len(batches[1]))
		}

		type result struct {
			item llmBatchAssessment
			err  error
			logs []string
		}

		results := make(chan result, len(batches))
		for i := range batches {
			go func(logChunk []string) {
				item, err := analyzeLogBatch(ctx, logChunk, ollamaEndpoint, llmInstance)
				results <- result{item: item, err: err, logs: logChunk}
			}(batches[i])
		}

		combined := llmBatchAssessment{
			Timestamp:     time.Now().Format(time.RFC3339),
			NormalLogs:    make([]string, 0, len(batch)),
			SuspicousLogs: make([]string, 0, len(batch)),
			DangerousLogs: make([]string, 0, len(batch)),
		}
		reasoningParts := make([]string, 0, len(batches))
		failedLogs := make([]string, 0, len(batch))
		for i := 0; i < len(batches); i++ {
			result := <-results
			if result.err != nil {
				fmt.Fprintf(os.Stderr, "LLM dispatch failed for %d logs: %v\n", len(result.logs), result.err)
				failedLogs = append(failedLogs, result.logs...)
				continue
			}
			combined.NormalLogs = append(combined.NormalLogs, result.item.NormalLogs...)
			combined.SuspicousLogs = append(combined.SuspicousLogs, result.item.SuspicousLogs...)
			combined.DangerousLogs = append(combined.DangerousLogs, result.item.DangerousLogs...)
			if strings.TrimSpace(result.item.Reasoning) != "" {
				reasoningParts = append(reasoningParts, result.item.Reasoning)
			}
		}

		if len(combined.NormalLogs)+len(combined.SuspicousLogs)+len(combined.DangerousLogs) == 0 {
			fmt.Println("LLM dispatch returned no parsed assessments.")
			if len(failedLogs) > 0 {
				pending = append(failedLogs, pending...)
				if oldest.IsZero() {
					oldest = time.Now()
				}
				retryAfter = time.Now().Add(llmRetryBackoff)
				fmt.Printf("LLM retry scheduled in %s for %d logs\n", llmRetryBackoff, len(failedLogs))
			}
			return
		}

		if len(failedLogs) > 0 {
			pending = append(failedLogs, pending...)
			if oldest.IsZero() {
				oldest = time.Now()
			}
			retryAfter = time.Now().Add(llmRetryBackoff)
			fmt.Printf("LLM partial success; retrying %d failed logs in %s\n", len(failedLogs), llmRetryBackoff)
		} else {
			retryAfter = time.Time{}
		}
		combined.Reasoning = strings.Join(reasoningParts, "\n\n")

		payload, err := json.MarshalIndent(combined, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to serialize LLM analysis: %v\n", err)
			return
		}

		total := len(combined.NormalLogs) + len(combined.SuspicousLogs) + len(combined.DangerousLogs)
		fmt.Printf("LLM response received for %d logs (normal=%d suspicious=%d dangerous=%d)\n",
			total, len(combined.NormalLogs), len(combined.SuspicousLogs), len(combined.DangerousLogs))
		for _, log := range combined.NormalLogs {
			fmt.Printf("  [NORMAL]    %s\n", log)
		}
		for _, log := range combined.SuspicousLogs {
			fmt.Printf("  [SUSPICIOUS] %s\n", log)
		}
		for _, log := range combined.DangerousLogs {
			fmt.Printf("  [DANGEROUS]  %s\n", log)
		}
		if strings.TrimSpace(combined.Reasoning) != "" {
			fmt.Printf("  Reasoning: %s\n", combined.Reasoning)
		}

		if err := appendLogBlock(analysisFile, analysisFileMu, string(payload)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to append LLM analysis to log: %v\n", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-logLines:
			if !ok {
				flush("shutdown", true)
				return
			}

			if len(pending) == 0 {
				oldest = time.Now()
			}
			pending = append(pending, line)

			if len(pending) >= llmBatchThreshold {
				flush("count>=25", false)
			}
		case <-ticker.C:
			if len(pending) > 0 && !oldest.IsZero() && time.Since(oldest) >= 1*time.Minute {
				flush("age>=1m", false)
			}
		}
	}
}

func analyzeLogBatch(ctx context.Context, logLines []string, ollamaEndpoint, llmInstance string) (llmBatchAssessment, error) {
	logsJSON, err := json.Marshal(logLines)
	if err != nil {
		return llmBatchAssessment{}, fmt.Errorf("unable to prepare log batch: %w", err)
	}

	prompt := fmt.Sprintf(
		"You are a security log analyst. Review the JSON array of logs below and return ONE JSON object only with this exact shape: {\"timestamp\":\"RFC3339\",\"reasoning\":\"...\",\"normalLogs\":[],\"suspicousLogs\":[],\"dangerousLogs\":[]}. Use only exact log lines from input arrays. Do not use markdown, code fences, or extra text. Return ONLY raw JSON.\\n\\nExample transaction:\\nInput logs: [\"[2026-04-26T20:00:00Z] EXEC: process pid=120 ppid=1 launched /usr/bin/vim and targeted /tmp/note.txt\",\"[2026-04-26T20:00:01Z] EXEC: process pid=121 ppid=1 launched /usr/bin/curl and targeted http://bad.test/payload\",\"[2026-04-26T20:00:02Z] EXEC: process pid=122 ppid=121 launched /tmp/payload and targeted /tmp/payload\"]\\nOutput JSON: {\"timestamp\":\"2026-04-26T20:00:03Z\",\"reasoning\":\"curl to external payload URL followed by execution from /tmp indicates likely malware staging.\",\"normalLogs\":[\"[2026-04-26T20:00:00Z] EXEC: process pid=120 ppid=1 launched /usr/bin/vim and targeted /tmp/note.txt\"],\"suspicousLogs\":[\"[2026-04-26T20:00:01Z] EXEC: process pid=121 ppid=1 launched /usr/bin/curl and targeted http://bad.test/payload\"],\"dangerousLogs\":[\"[2026-04-26T20:00:02Z] EXEC: process pid=122 ppid=121 launched /tmp/payload and targeted /tmp/payload\"]}\\n\\nNow analyze this input and return ONLY raw JSON.\\n\\nLogs: %s",
		string(logsJSON),
	)

	payload := map[string]any{
		"model":  llmInstance,
		"prompt": prompt,
		"stream": false,
	}

	requestBody, err := json.Marshal(payload)
	if err != nil {
		return llmBatchAssessment{}, fmt.Errorf("unable to build Ollama request: %w", err)
	}

	requestURL := ollamaGenerateURL(ollamaEndpoint)
	fmt.Printf("LLM request start: endpoint=%s model=%s logs=%d timeout=none\n", requestURL, llmInstance, len(logLines))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(requestBody))
	if err != nil {
		return llmBatchAssessment{}, fmt.Errorf("unable to create Ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return llmBatchAssessment{}, fmt.Errorf("unable to call Ollama endpoint %s: %w", requestURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return llmBatchAssessment{}, fmt.Errorf("unable to read Ollama response: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return llmBatchAssessment{}, fmt.Errorf("Ollama API returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Response string `json:"response"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return llmBatchAssessment{}, fmt.Errorf("unable to parse Ollama response JSON: %w", err)
	}

	if strings.TrimSpace(parsed.Error) != "" {
		return llmBatchAssessment{}, errors.New(parsed.Error)
	}

	jsonPayload, err := extractJSONObjectPayload(parsed.Response)
	if err != nil {
		return fallbackBatchAssessment(logLines, parsed.Response), nil
	}

	var assessment llmBatchAssessment
	if err := json.Unmarshal([]byte(jsonPayload), &assessment); err != nil {
		return fallbackBatchAssessment(logLines, parsed.Response), nil
	}

	if strings.TrimSpace(assessment.Timestamp) == "" {
		assessment.Timestamp = time.Now().Format(time.RFC3339)
	}

	if len(assessment.NormalLogs)+len(assessment.SuspicousLogs)+len(assessment.DangerousLogs) == 0 {
		return fallbackBatchAssessment(logLines, parsed.Response), nil
	}

	return assessment, nil
}

func fallbackBatchAssessment(logLines []string, rawResponse string) llmBatchAssessment {
	reason := strings.TrimSpace(rawResponse)
	if reason == "" {
		reason = "LLM returned no structured JSON response; defaulted all logs to suspicious."
	} else {
		reason = "LLM returned non-JSON response; defaulted all logs to suspicious. Raw response: " + reason
	}

	return llmBatchAssessment{
		Timestamp:     time.Now().Format(time.RFC3339),
		Reasoning:     reason,
		NormalLogs:    []string{},
		SuspicousLogs: append([]string(nil), logLines...),
		DangerousLogs: []string{},
	}
}

func extractJSONObjectPayload(response string) (string, error) {
	trimmed := strings.TrimSpace(response)
	if trimmed == "" {
		return "", errors.New("LLM returned empty response")
	}

	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start == -1 || end == -1 || end < start {
		return "", fmt.Errorf("LLM response did not include a JSON object: %s", trimmed)
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
