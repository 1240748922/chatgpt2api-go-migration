// Package replenish launches the original registration tool without a shell.
package replenish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Settings struct {
	Enabled        bool     `json:"enabled"`
	Source         string   `json:"registration_source"`
	Mode           string   `json:"registration_mode"`
	Minimum        int      `json:"minimum_available"`
	Target         int      `json:"target_available"`
	MaxBatch       int      `json:"max_batch_size"`
	Workers        int      `json:"workers"`
	Interval       int      `json:"interval_seconds"`
	Cooldown       int      `json:"cooldown_seconds"`
	Timeout        int      `json:"launch_timeout_seconds"`
	MailboxFile    string   `json:"mailbox_file"`
	CFDomain       string   `json:"cfworker_domain"`
	SmailrDomain   string   `json:"smailr_domain"`
	RemailMode     string   `json:"remail_service_mode"`
	RemailSupply   string   `json:"remail_supply"`
	RemailSuffix   string   `json:"remail_email_suffix"`
	RemailProject  int      `json:"remail_project_id"`
	No2FA          bool     `json:"no_2fa"`
	NoPhoneReuse   bool     `json:"no_phone_reuse"`
	ImportSessions bool     `json:"import_new_sessions"`
	ExtraArgs      []string `json:"extra_args"`
}

func Defaults() Settings {
	return Settings{Source: "configured", Mode: "password", Minimum: 100, Target: 500, MaxBatch: 100, Workers: 2, Interval: 60, Cooldown: 300, Timeout: 3600, RemailMode: "purchase", RemailSupply: "private_first", RemailSuffix: "outlook.com", RemailProject: 2, No2FA: true, NoPhoneReuse: true, ImportSessions: true}
}
func (s *Settings) Validate() error {
	if !oneOf(s.Source, "configured", "mailbox_file", "cfworker", "smailr", "phone", "remail_target") || !oneOf(s.Mode, "password", "passwordless", "har", "legacy") {
		return errors.New("无效的注册来源或模式")
	}
	if s.Minimum < 0 || s.Target < s.Minimum || s.Target > 100000 || s.MaxBatch < 1 || s.MaxBatch > 1000 || s.Workers < 1 || s.Workers > 16 || s.Interval < 5 || s.Cooldown < 5 || s.Timeout < 30 || s.Timeout > 86400 {
		return errors.New("注册数量、并发或时间设置超出范围")
	}
	if s.Source == "mailbox_file" && s.MailboxFile == "" {
		return errors.New("请先上传邮箱文件")
	}
	for _, arg := range s.ExtraArgs {
		if strings.HasPrefix(arg, "--output") || strings.HasPrefix(arg, "--registration-batch") || strings.HasPrefix(arg, "--config") || strings.HasPrefix(arg, "--mailbox-file") {
			return errors.New("附加参数不能覆盖输出、批次或配置路径")
		}
	}
	return nil
}
func oneOf(v string, options ...string) bool {
	for _, o := range options {
		if o == v {
			return true
		}
	}
	return false
}
func Command(s Settings, count int, entry, output, id string) []string {
	args := []string{entry, "--registration-mode", s.Mode}
	if s.Source == "remail_target" {
		args = append(args, "--target-at200", strconv.Itoa(count), "--buy-remail-mailbox", "--remail-service-mode", s.RemailMode, "--remail-supply", s.RemailSupply, "--remail-email-suffix", s.RemailSuffix, "--remail-project-id", strconv.Itoa(s.RemailProject))
	} else {
		args = append(args, "--count", strconv.Itoa(count))
		switch s.Source {
		case "mailbox_file":
			args = append(args, "--mailbox-file", s.MailboxFile, "--registration-at-only")
		case "cfworker":
			args = append(args, "--buy-cfworker-mailbox", "--cfworker-domain", s.CFDomain, "--registration-at-only")
		case "smailr":
			args = append(args, "--buy-smailr-mailbox", "--smailr-domain", s.SmailrDomain, "--registration-at-only")
		case "phone":
			args = append(args, "--phone-register")
		}
	}
	args = append(args, "--workers", strconv.Itoa(s.Workers), "--output-dir", output, "--registration-batch-id", id)
	if s.No2FA {
		args = append(args, "--no-2fa")
	}
	if s.NoPhoneReuse && s.Source != "phone" {
		args = append(args, "--no-phone-reuse")
	}
	return append(args, s.ExtraArgs...)
}

var secretPattern = regexp.MustCompile(`(?i)((?:access_token|refresh_token|id_token|api_key|password|authorization|admin_token|api_token)["'\s:=]+)[^\s,}"']+`)
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
var rtPattern = regexp.MustCompile(`\brt\.[A-Za-z0-9._-]+`)
var proxyPattern = regexp.MustCompile(`(?i)(https?|socks5h?)://[^\s/@]+:[^\s/@]+@`)

func Redact(line string) string {
	line = secretPattern.ReplaceAllString(line, "${1}[redacted]")
	line = jwtPattern.ReplaceAllString(line, "[redacted]")
	line = rtPattern.ReplaceAllString(line, "[redacted]")
	return proxyPattern.ReplaceAllString(line, "${1}://[redacted]@")
}

// LogWriter bounds lines and serializes stdout/stderr, persisting only sanitized text.
type LogWriter struct {
	Mu      sync.Mutex
	File    io.Writer
	Line    func(string)
	secrets []string
	pending []byte
}

func NewLogWriter(file io.Writer, config map[string]any, onLine func(string)) *LogWriter {
	w := &LogWriter{File: file, Line: onLine}
	var walk func(any, string)
	walk = func(v any, key string) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				walk(v, k)
			}
		case []any:
			for _, v := range x {
				walk(v, key)
			}
		case []string:
			for _, v := range x {
				walk(v, key)
			}
		case string:
			key = strings.ToLower(key)
			if len(x) > 5 && (strings.Contains(key, "token") || strings.Contains(key, "password") || strings.Contains(key, "key") || key == "registration" || key == "default" || key == "pool") {
				w.secrets = append(w.secrets, x)
			}
		}
	}
	walk(config, "")
	return w
}
func (w *LogWriter) Write(p []byte) (int, error) {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		i := strings.IndexByte(string(w.pending), '\n')
		if i < 0 {
			if len(w.pending) > 16384 {
				w.emit(w.pending[:16384])
				w.pending = nil
			}
			break
		}
		w.emit(w.pending[:i])
		w.pending = w.pending[i+1:]
	}
	return len(p), nil
}
func (w *LogWriter) emit(p []byte) {
	line := string(p)
	for _, secret := range w.secrets {
		line = strings.ReplaceAll(line, secret, "[redacted]")
	}
	line = Redact(line)
	if len(line) > 4096 {
		line = line[:4096] + "…"
	}
	if w.File != nil {
		fmt.Fprintln(w.File, line)
	}
	if w.Line != nil {
		w.Line(line)
	}
}
func (w *LogWriter) Flush() {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	if len(w.pending) > 0 {
		w.emit(w.pending)
		w.pending = nil
	}
}

func Run(ctx context.Context, python, workdir, configPath string, args []string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "REGISTER_CONFIG_PATH="+configPath, "PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1")
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = 2 * time.Second
	configureProcessTree(cmd)
	return cmd.Run()
}

func Sessions(output, id string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(output, "session_*.json"))
	if err != nil {
		return nil, err
	}
	result := []string{}
	for _, path := range files {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 10<<20 {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var d map[string]any
		if json.Unmarshal(raw, &d) != nil {
			continue
		}
		batch, _ := d["registration_batch_id"].(string)
		if batch == "" {
			batch, _ = d["batch_id"].(string)
		}
		if batch != "" && batch != id {
			continue
		}
		result = append(result, string(raw))
		if len(result) == 1000 {
			break
		}
	}
	return result, nil
}
