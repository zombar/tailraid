package nfsgw

import (
	"fmt"
	"log/slog"
	"strings"

	nfs "github.com/willscott/go-nfs"
)

// UseLogger routes the go-nfs library's log lines through l so the daemon's log stream
// has one format (docs/07). Library errors are client-protocol noise (bad handles, aborted
// connections) and land at debug; warnings and panics keep their level.
func UseLogger(l *slog.Logger) {
	nfs.Log = &slogNFSLogger{l: l.With("src", "nfs")}
}

type slogNFSLogger struct {
	l     *slog.Logger
	level nfs.LogLevel
}

func (s *slogNFSLogger) SetLevel(level nfs.LogLevel) { s.level = level }
func (s *slogNFSLogger) GetLevel() nfs.LogLevel      { return s.level }
func (s *slogNFSLogger) ParseLevel(level string) (nfs.LogLevel, error) {
	switch strings.ToLower(level) {
	case "panic":
		return nfs.PanicLevel, nil
	case "fatal":
		return nfs.FatalLevel, nil
	case "error":
		return nfs.ErrorLevel, nil
	case "warn", "warning":
		return nfs.WarnLevel, nil
	case "info":
		return nfs.InfoLevel, nil
	case "debug":
		return nfs.DebugLevel, nil
	case "trace":
		return nfs.TraceLevel, nil
	}
	return 0, fmt.Errorf("nfsgw: unknown log level %q", level)
}

func msg(args ...any) string { return strings.TrimSpace(fmt.Sprint(args...)) }

func (s *slogNFSLogger) Panic(args ...any) { s.l.Error(msg(args...)); panic(msg(args...)) }
func (s *slogNFSLogger) Fatal(args ...any) { s.l.Error(msg(args...)) }
func (s *slogNFSLogger) Error(args ...any) { s.l.Debug(msg(args...)) }
func (s *slogNFSLogger) Warn(args ...any)  { s.l.Warn(msg(args...)) }
func (s *slogNFSLogger) Info(args ...any)  { s.l.Info(msg(args...)) }
func (s *slogNFSLogger) Debug(args ...any) { s.l.Debug(msg(args...)) }
func (s *slogNFSLogger) Trace(args ...any) { s.l.Debug(msg(args...)) }
func (s *slogNFSLogger) Print(args ...any) { s.l.Info(msg(args...)) }

func (s *slogNFSLogger) Panicf(format string, args ...any) {
	s.l.Error(fmt.Sprintf(format, args...))
	panic(fmt.Sprintf(format, args...))
}

func (s *slogNFSLogger) Fatalf(format string, args ...any) {
	s.l.Error(fmt.Sprintf(format, args...))
}

func (s *slogNFSLogger) Errorf(format string, args ...any) {
	s.l.Debug(fmt.Sprintf(format, args...))
}

func (s *slogNFSLogger) Warnf(format string, args ...any) {
	s.l.Warn(fmt.Sprintf(format, args...))
}

func (s *slogNFSLogger) Infof(format string, args ...any) {
	s.l.Info(fmt.Sprintf(format, args...))
}

func (s *slogNFSLogger) Debugf(format string, args ...any) {
	s.l.Debug(fmt.Sprintf(format, args...))
}

func (s *slogNFSLogger) Tracef(format string, args ...any) {
	s.l.Debug(fmt.Sprintf(format, args...))
}

func (s *slogNFSLogger) Printf(format string, args ...any) {
	s.l.Info(fmt.Sprintf(format, args...))
}
