// Command agui-check consumes an AG-UI endpoint via the AG-UI Go SDK
// client packages and reports protocol compliance: exit 0 when the stream
// decodes cleanly through the SDK decoder with invariants intact.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
)

func main() {
	url := flag.String("url", "http://localhost:8090/ag-ui", "AG-UI endpoint URL")
	message := flag.String("message", "Reply with exactly: hello", "user message")
	thread := flag.String("thread", "agui-check", "thread id")
	timeout := flag.Duration("timeout", 5*time.Minute, "run timeout")
	flag.Parse()

	input := map[string]any{
		"threadId": *thread,
		"runId":    fmt.Sprintf("check-%d", time.Now().UnixMilli()),
		"messages": []map[string]any{{
			"id":      "m1",
			"role":    "user",
			"content": *message,
		}},
	}
	body, _ := json.Marshal(input)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", *url, strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "status: %d\n", resp.StatusCode)
		os.Exit(1)
	}

	decoder := events.NewEventDecoder(nil)
	sawStarted, sawFinished := false, false
	var events_ []string
	var collected []events.Event
	buf := make([]byte, 0, 64*1024)
	chunk := make([]byte, 64*1024)
	for {
		n, err := resp.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			idx := indexByte(buf, '\n')
			if idx < 0 {
				break
			}
			line := string(buf[:idx])
			buf = buf[idx+1:]
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			var raw map[string]any
			if err := json.Unmarshal([]byte(data), &raw); err != nil {
				fmt.Fprintf(os.Stderr, "non-JSON data line: %s\n", data)
				os.Exit(1)
			}
			evType, _ := raw["type"].(string)
			ev, err := decoder.DecodeEvent(evType, []byte(data))
			if err != nil {
				fmt.Fprintf(os.Stderr, "DECODE FAIL [%s]: %v\n", evType, err)
				os.Exit(1)
			}
			events_ = append(events_, evType)
			collected = append(collected, ev)
			switch e := ev.(type) {
			case *events.RunStartedEvent:
				sawStarted = true
			case *events.RunFinishedEvent:
				sawFinished = true
			case *events.RunErrorEvent:
				fmt.Fprintf(os.Stderr, "RUN_ERROR: %v\n", e.Message)
			}
		}
		if n == 0 {
			if err != nil {
				break
			}
			continue
		}
		if err != nil {
			break
		}
	}

	if !sawStarted {
		fmt.Fprintln(os.Stderr, "FAIL: no RUN_STARTED")
		os.Exit(1)
	}
	if !sawFinished {
		fmt.Fprintln(os.Stderr, "FAIL: no RUN_FINISHED")
		os.Exit(1)
	}
	// Sequence-level compliance via the SDK's validator.
	if err := events.ValidateSequence(collected); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: sequence: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK: %d events decoded cleanly and ValidateSequence passed: %s\n", len(events_), strings.Join(events_, " "))
}

// indexByte finds the first occurrence of b in s.
func indexByte(s []byte, b byte) int {
	for i, c := range s {
		if c == b {
			return i
		}
	}
	return -1
}
