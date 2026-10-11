package main

import (
	"fmt"
	"io"
	"time"
)

const ciLogPath = "/var/log/cloud-init.log"

type ciLog struct {
	file   io.Writer
	output io.Writer
	status *statusFile
}

func (l *ciLog) write(module, level, msg string) {
	source := "main.py"
	if module != "" {
		source = "cc_" + module + ".py"
	}
	line := fmt.Sprintf("%s - %s[%s]: %s\n", time.Now().UTC().Format("2006-01-02 15:04:05,000"), source, level, msg)
	_, _ = io.WriteString(l.file, line)
	if level == "DEBUG" || level == "INFO" {
		return
	}
	_, _ = io.WriteString(l.output, line)
	l.status.recoverable(level, msg)
}

func (l *ciLog) debug(module, msg string) { l.write(module, "DEBUG", msg) }
func (l *ciLog) info(module, msg string)  { l.write(module, "INFO", msg) }
func (l *ciLog) warn(module, msg string)  { l.write(module, "WARNING", msg) }

func (l *ciLog) notice(module string, n notice) { l.write(module, n.level, n.text) }

func (l *ciLog) fail(module string, err error) {
	l.status.fail(fmt.Sprintf("('%s', '%v')", module, err))
	l.warn(module, fmt.Sprintf("Running module %s failed: %v", module, err))
}

type notice struct {
	level string
	text  string
}

func warning(format string, args ...any) notice {
	return notice{level: "WARNING", text: fmt.Sprintf(format, args...)}
}
