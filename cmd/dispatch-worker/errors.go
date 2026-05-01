// errors.go — split error logging so we can triage worker vs endpoint
// failures independently. Worker errors are things the per-message pipeline
// hits (Graph 429, PDF parse failed, the ERP query timeout); endpoint errors
// are things the AI endpoints hit (HTTP 500, connection refused, Ollama
// returned empty). Both tee to stdout so the main log stays whole.

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// errLog is the worker-wide error sink. Set in main; read by the per-message
// storeErr closures so they can tee a one-line summary to the extraction
// log file. Package-level because the closures are deep in extractInvoice /
// verifyInvoice / openExtract / processFallbackJob — threading it through
// every signature would churn 4 functions and 6+ call sites for no benefit.
var errLog *errorLoggers

// errorLoggers holds optional file handles for each error stream. Any can
// be nil (file open failed) — we just skip the tee in that case.
//
// Three streams:
//   - worker:     worker-level pipeline errors (Graph 429, PDF parse, the ERP timeout)
//   - endpoint:   per-request errors against AI endpoints (HTTP 500, conn refused)
//   - extraction: per-message extraction failures with subject + model + error,
//                 designed to be tail-able / grep-able when triaging a batch
type errorLoggers struct {
	worker     *os.File
	endpoint   *os.File
	extraction *os.File
	mu         sync.Mutex
}

func (l *errorLoggers) close() {
	if l == nil {
		return
	}
	if l.worker != nil {
		_ = l.worker.Close()
	}
	if l.endpoint != nil {
		_ = l.endpoint.Close()
	}
	if l.extraction != nil {
		_ = l.extraction.Close()
	}
}

// logWorkerErr writes a worker-level error to stdout + the worker-errors
// file. Format matches the rest of the worker's log output (prefix-free,
// with a timestamp since this might be grepped out of context).
func (l *errorLoggers) logWorkerErr(format string, args ...interface{}) {
	if l == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	line := time.Now().UTC().Format("2006-01-02T15:04:05Z ") + msg + "\n"
	fmt.Print(line)
	l.mu.Lock()
	if l.worker != nil {
		_, _ = io.WriteString(l.worker, line)
	}
	l.mu.Unlock()
}

// logExtractionErr appends one extraction failure to the extraction-errors
// file in pipe-separated form: TIMESTAMP | MSG_ID | SUBJECT | MODEL | ERROR.
// Each field is single-line (newlines collapsed to spaces) and capped so
// the file stays grep-able. Both stdout and file get the line; if either
// the file handle is nil or the write fails we silently skip — extraction
// errors are also captured in invoice_extractions.error_msg, so this file
// is convenience, not source of truth.
func (l *errorLoggers) logExtractionErr(msgID, subject, model, errMsg string) {
	if l == nil {
		return
	}
	clean := func(s string, max int) string {
		s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ", "|", "/").Replace(s)
		s = strings.TrimSpace(s)
		if len(s) > max {
			s = s[:max] + "…"
		}
		return s
	}
	line := time.Now().UTC().Format("2006-01-02T15:04:05Z") + " | " +
		clean(msgID, 80) + " | " +
		clean(subject, 120) + " | " +
		clean(model, 40) + " | " +
		clean(errMsg, 300) + "\n"
	fmt.Print(line)
	l.mu.Lock()
	if l.extraction != nil {
		_, _ = io.WriteString(l.extraction, line)
	}
	l.mu.Unlock()
}

// logEndpointErr writes an endpoint-level error (e.g., from the aiclass
// EndpointHook OnError path) to stdout + endpoint-errors. Takes the URL
// explicitly so we can group/filter by host later.
func (l *errorLoggers) logEndpointErr(url, format string, args ...interface{}) {
	if l == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	line := time.Now().UTC().Format("2006-01-02T15:04:05Z ") + url + " " + msg + "\n"
	fmt.Print(line)
	l.mu.Lock()
	if l.endpoint != nil {
		_, _ = io.WriteString(l.endpoint, line)
	}
	l.mu.Unlock()
}
