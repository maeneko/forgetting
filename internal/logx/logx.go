// Package logx — простой логгер: человекочитаемые строки в консоль и JSON-строки в
// файл (как winston в Node-версии: console «<ts> level: msg», файл {level,message,timestamp,…}).
package logx

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger struct {
	mu   sync.Mutex
	out  io.Writer
	file *os.File
}

// New пишет в stdout и (если dir не пуст) в dir/app.log.
func New(out io.Writer, dir string) *Logger {
	l := &Logger{out: out}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if f, err := os.OpenFile(filepath.Join(dir, "app.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				l.file = f
			}
		}
	}
	return l
}

func (l *Logger) log(level, msg string, kv []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if l.out != nil {
		fmt.Fprintf(l.out, "%s %s: %s\n", ts, level, msg)
	}
	if l.file != nil {
		m := map[string]any{"level": level, "message": msg, "timestamp": ts}
		for i := 0; i+1 < len(kv); i += 2 {
			if k, ok := kv[i].(string); ok {
				if e, isErr := kv[i+1].(error); isErr {
					m[k] = e.Error()
				} else {
					m[k] = kv[i+1]
				}
			}
		}
		b, _ := json.Marshal(m)
		l.file.Write(append(b, '\n'))
	}
}

func (l *Logger) Info(msg string, kv ...any)  { l.log("info", msg, kv) }
func (l *Logger) Warn(msg string, kv ...any)  { l.log("warn", msg, kv) }
func (l *Logger) Error(msg string, kv ...any) { l.log("error", msg, kv) }
func (l *Logger) Debug(msg string, kv ...any) { l.log("debug", msg, kv) }
