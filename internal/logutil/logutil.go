/**
 * 日志初始化模块
 * 负责配置控制台输出、文件轮换、错误日志分流与格式化策略
 */
package logutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"codex-proxy/internal/config"

	isatty "github.com/mattn/go-isatty"
	log "github.com/sirupsen/logrus"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

const (
	defaultTimestampFormat = "2006-01-02T15:04:05.000000Z07:00"
	defaultAppLogName      = "app.log"
	defaultErrorLogName    = "error.log"
)

/**
 * writerHook 将指定等级的日志格式化后写入目标 writer。
 */
type writerHook struct {
	formatter log.Formatter
	writer    io.Writer
	levels    []log.Level
}

/**
 * Levels 返回该 hook 处理的日志等级。
 * @returns []log.Level - 支持的日志等级
 */
func (h *writerHook) Levels() []log.Level {
	return h.levels
}

/**
 * Fire 格式化日志并写入目标 writer。
 * @param entry - 当前日志条目
 * @returns error - 写入失败时返回错误
 */
func (h *writerHook) Fire(entry *log.Entry) error {
	dup := entry.Dup()
	line, err := h.formatter.Format(dup)
	if err != nil {
		return err
	}
	_, err = h.writer.Write(line)
	return err
}

/**
 * Setup 按配置初始化全局日志器。
 * @param cfg - 应用配置
 * @returns error - 初始化失败时返回错误
 */
func Setup(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("日志初始化失败: 配置为空")
	}

	logger := log.StandardLogger()
	level, err := log.ParseLevel(cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("日志初始化失败: 无效日志等级 %q", cfg.LogLevel)
	}

	if err := os.MkdirAll(cfg.LogDir, 0o700); err != nil {
		return fmt.Errorf("日志初始化失败: 创建日志目录失败: %w", err)
	}

	appPath := filepath.Join(cfg.LogDir, defaultAppLogName)
	if err := ensureLogFile(appPath); err != nil {
		return err
	}

	consoleFormatter := buildFormatter(cfg, false)
	fileFormatter := buildFormatter(cfg, true)

	logger.SetLevel(level)
	logger.SetReportCaller(cfg.LogReportCaller)
	logger.SetFormatter(consoleFormatter)
	logger.SetOutput(os.Stdout)
	logger.ReplaceHooks(make(log.LevelHooks))

	appWriter := &lumberjack.Logger{
		Filename:   appPath,
		MaxSize:    cfg.LogMaxSizeMB,
		MaxBackups: cfg.LogMaxBackups,
		MaxAge:     cfg.LogMaxAgeDays,
		LocalTime:  true,
		Compress:   cfg.LogCompress,
	}
	logger.AddHook(&writerHook{
		formatter: fileFormatter,
		writer:    appWriter,
		levels:    []log.Level{log.DebugLevel, log.InfoLevel, log.WarnLevel},
	})

	if cfg.LogSeparateErrorFile {
		errorPath := filepath.Join(cfg.LogDir, defaultErrorLogName)
		if err := ensureLogFile(errorPath); err != nil {
			return err
		}
		errorWriter := &lumberjack.Logger{
			Filename:   errorPath,
			MaxSize:    cfg.LogMaxSizeMB,
			MaxBackups: cfg.LogMaxBackups,
			MaxAge:     cfg.LogMaxAgeDays,
			LocalTime:  true,
			Compress:   cfg.LogCompress,
		}
		logger.AddHook(&writerHook{
			formatter: fileFormatter,
			writer:    errorWriter,
			levels:    []log.Level{log.ErrorLevel, log.FatalLevel, log.PanicLevel},
		})
	} else {
		logger.AddHook(&writerHook{
			formatter: fileFormatter,
			writer:    appWriter,
			levels:    []log.Level{log.ErrorLevel, log.FatalLevel, log.PanicLevel},
		})
	}

	return nil
}

/**
 * ensureLogFile 确保日志文件存在且权限为 0600。
 * @param path - 日志文件路径
 * @returns error - 创建或设置权限失败时返回错误
 */
func ensureLogFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("日志初始化失败: 打开日志文件 %s 失败: %w", path, err)
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("日志初始化失败: 关闭日志文件 %s 失败: %w", path, closeErr)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("日志初始化失败: 设置日志文件权限 %s 失败: %w", path, err)
	}
	return nil
}

/**
 * buildFormatter 根据配置构建控制台或文件 formatter。
 * @param cfg - 应用配置
 * @param forFile - true 表示文件输出，false 表示控制台输出
 * @returns log.Formatter - formatter 实例
 */
func buildFormatter(cfg *config.Config, forFile bool) log.Formatter {
	callerPrettyfier := func(frame *runtime.Frame) (function string, file string) {
		if frame == nil {
			return "", ""
		}
		function = filepath.Base(frame.Function)
		file = fmt.Sprintf("%s:%d", filepath.Base(frame.File), frame.Line)
		return function, file
	}

	if cfg.LogFormat == "json" {
		return &log.JSONFormatter{
			TimestampFormat:  defaultTimestampFormat,
			CallerPrettyfier: callerPrettyfier,
		}
	}

	formatter := &log.TextFormatter{
		FullTimestamp:          true,
		TimestampFormat:        defaultTimestampFormat,
		DisableLevelTruncation: true,
		PadLevelText:           true,
		CallerPrettyfier:       callerPrettyfier,
	}
	if forFile {
		formatter.DisableColors = true
		return formatter
	}
	applyConsoleColor(formatter, cfg.LogColor)
	return formatter
}

/**
 * applyConsoleColor 根据配置决定控制台颜色策略。
 * @param formatter - text formatter
 * @param mode - auto|always|never
 */
func applyConsoleColor(formatter *log.TextFormatter, mode string) {
	if formatter == nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "always":
		formatter.ForceColors = true
		formatter.DisableColors = false
	case "never":
		formatter.DisableColors = true
		formatter.ForceColors = false
	default:
		formatter.DisableColors = !(isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()))
	}
}
