package main

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// level is an ordered log severity.
type level int

const (
	levelDebug level = iota
	levelInfo
	levelWarn
	levelError
)

var levelNames = map[level]string{
	levelDebug: "DEBUG",
	levelInfo:  "INFO",
	levelWarn:  "WARN",
	levelError: "ERROR",
}

// logger is a tiny leveled logger over the stdlib log package.
type logger struct {
	min level
	l   *log.Logger
}

// newLogger builds a logger from a level name (debug|info|warn|error).
// Unknown/empty names default to info.
func newLogger(levelName string) *logger {
	min := levelInfo
	switch strings.ToLower(strings.TrimSpace(levelName)) {
	case "debug":
		min = levelDebug
	case "info", "":
		min = levelInfo
	case "warn", "warning":
		min = levelWarn
	case "error":
		min = levelError
	}
	return &logger{min: min, l: log.New(os.Stderr, "", log.LstdFlags)}
}

func (lg *logger) log(lvl level, format string, args ...any) {
	if lvl < lg.min {
		return
	}
	lg.l.Printf("[%s] %s", levelNames[lvl], fmt.Sprintf(format, args...))
}

func (lg *logger) debugf(format string, args ...any) { lg.log(levelDebug, format, args...) }
func (lg *logger) infof(format string, args ...any)  { lg.log(levelInfo, format, args...) }
func (lg *logger) warnf(format string, args ...any)  { lg.log(levelWarn, format, args...) }
func (lg *logger) errorf(format string, args ...any) { lg.log(levelError, format, args...) }
