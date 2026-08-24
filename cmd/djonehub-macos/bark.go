package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	barkConfigVersion             = 2
	barkMessagePlaceholder        = "{message}"
	defaultSMSBarkTemplate        = "{alias_prefix}{content}"
	defaultMissedCallBarkTemplate = "{alias_prefix}未接来电：{number}\n时间：{started_at}"
	defaultCallBarkTemplate       = "{alias_prefix}{status}：{number}\n时间：{started_at}"
)

type barkChannelSettings struct {
	Enabled         bool   `json:"enabled"`
	APIURL          string `json:"api_url"`
	Alias           string `json:"alias"`
	MessageTemplate string `json:"message_template"`
}

type barkSettings struct {
	Version    int                 `json:"version"`
	SMS        barkChannelSettings `json:"sms"`
	MissedCall barkChannelSettings `json:"missed_call"`
}

type barkSettingsDocument struct {
	Version    int                  `json:"version"`
	SMS        *barkChannelSettings `json:"sms"`
	MissedCall *barkChannelSettings `json:"missed_call"`

	// Version 1 used a single flat channel. It migrates to SMS only.
	Enabled bool   `json:"enabled"`
	APIURL  string `json:"api_url"`
	Alias   string `json:"alias"`
}

type barkChannelKind string

const (
	barkChannelSMS        barkChannelKind = "sms"
	barkChannelMissedCall barkChannelKind = "missed_call"
)

var barkTemplateVariablePattern = regexp.MustCompile(`\{[a-z_]+\}`)

func normalizeBarkChannelSettings(settings barkChannelSettings, defaultTemplate string) barkChannelSettings {
	settings.APIURL = strings.TrimSpace(settings.APIURL)
	settings.Alias = strings.TrimSpace(settings.Alias)
	settings.MessageTemplate = strings.TrimSpace(settings.MessageTemplate)
	if settings.MessageTemplate == "" {
		settings.MessageTemplate = defaultTemplate
	}
	return settings
}

func normalizeBarkSettings(settings barkSettings) barkSettings {
	settings.Version = barkConfigVersion
	settings.SMS = normalizeBarkChannelSettings(settings.SMS, defaultSMSBarkTemplate)
	settings.MissedCall = normalizeBarkChannelSettings(settings.MissedCall, defaultCallBarkTemplate)
	if settings.MissedCall.MessageTemplate == defaultMissedCallBarkTemplate {
		settings.MissedCall.MessageTemplate = defaultCallBarkTemplate
	}
	return settings
}

func barkChannelLabel(kind barkChannelKind) string {
	if kind == barkChannelMissedCall {
		return "来电"
	}
	return "短信"
}

func barkChannelDefaultTemplate(kind barkChannelKind) string {
	if kind == barkChannelMissedCall {
		return defaultCallBarkTemplate
	}
	return defaultSMSBarkTemplate
}

func barkChannelAllowedVariables(kind barkChannelKind) map[string]bool {
	common := map[string]bool{
		"{alias}":        true,
		"{alias_prefix}": true,
	}
	if kind == barkChannelMissedCall {
		common["{number}"] = true
		common["{started_at}"] = true
		common["{ended_at}"] = true
		common["{duration}"] = true
		common["{status}"] = true
		return common
	}
	common["{sender}"] = true
	common["{content}"] = true
	common["{code}"] = true
	common["{timestamp}"] = true
	return common
}

func validateBarkURL(raw string, enabled bool, label string) error {
	if raw == "" {
		if enabled {
			return fmt.Errorf("启用%s Bark 通知前请填写 API 链接", label)
		}
		return nil
	}
	if strings.Count(raw, barkMessagePlaceholder) != 1 {
		return fmt.Errorf("%s Bark API 链接必须包含一个 %s 占位符", label, barkMessagePlaceholder)
	}
	if len(raw) > 2048 {
		return fmt.Errorf("%s Bark API 链接过长", label)
	}
	placeholderAt := strings.Index(raw, barkMessagePlaceholder)
	if separatorAt := strings.IndexAny(raw, "?#"); separatorAt >= 0 && placeholderAt > separatorAt {
		return fmt.Errorf("%s 占位符必须位于 Bark API 的路径中", barkMessagePlaceholder)
	}
	parsed, err := url.Parse(strings.Replace(raw, barkMessagePlaceholder, "test", 1))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s Bark API 链接必须是有效的 HTTP 或 HTTPS 地址", label)
	}
	return nil
}

func validateBarkChannelSettings(kind barkChannelKind, settings barkChannelSettings) error {
	settings = normalizeBarkChannelSettings(settings, barkChannelDefaultTemplate(kind))
	label := barkChannelLabel(kind)
	if err := validateBarkURL(settings.APIURL, settings.Enabled, label); err != nil {
		return err
	}
	if utf8.RuneCountInString(settings.Alias) > 80 {
		return fmt.Errorf("%s Bark 自定义别名不能超过 80 个字符", label)
	}
	if utf8.RuneCountInString(settings.MessageTemplate) > 4000 {
		return fmt.Errorf("%s Bark 消息模板不能超过 4000 个字符", label)
	}
	allowed := barkChannelAllowedVariables(kind)
	for _, variable := range barkTemplateVariablePattern.FindAllString(settings.MessageTemplate, -1) {
		if !allowed[variable] {
			return fmt.Errorf("%s Bark 消息模板包含不支持的变量 %s", label, variable)
		}
	}
	return nil
}

func validateBarkSettings(settings barkSettings) error {
	settings = normalizeBarkSettings(settings)
	if err := validateBarkChannelSettings(barkChannelSMS, settings.SMS); err != nil {
		return err
	}
	return validateBarkChannelSettings(barkChannelMissedCall, settings.MissedCall)
}

func formatBarkAliasPrefix(alias string) string {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return ""
	}
	if (strings.HasPrefix(alias, "[") && strings.HasSuffix(alias, "]")) ||
		(strings.HasPrefix(alias, "【") && strings.HasSuffix(alias, "】")) {
		return alias
	}
	return "[" + alias + "]"
}

func renderBarkTemplate(template string, values map[string]string) string {
	replacements := make([]string, 0, len(values)*2)
	for key, value := range values {
		replacements = append(replacements, "{"+key+"}", value)
	}
	return strings.NewReplacer(replacements...).Replace(template)
}

func renderSMSBarkMessage(settings barkChannelSettings, message receivedSMS) string {
	return renderBarkTemplate(settings.MessageTemplate, map[string]string{
		"alias":        settings.Alias,
		"alias_prefix": formatBarkAliasPrefix(settings.Alias),
		"sender":       message.Sender,
		"content":      message.Content,
		"code":         message.Code,
		"timestamp":    message.Timestamp.Local().Format("2006-01-02 15:04:05"),
	})
}

func renderCallBarkMessage(settings barkChannelSettings, call callRecord) string {
	endedAt := ""
	duration := ""
	if call.EndedAt != nil {
		endedAt = call.EndedAt.Local().Format("2006-01-02 15:04:05")
		duration = formatCallDuration(call.EndedAt.Sub(call.StartedAt))
	}
	number := strings.TrimSpace(call.Number)
	if number == "" {
		number = "未知号码"
	}
	status := "未接来电"
	if call.AIHandled {
		status = "AI 已接听"
	}
	return renderBarkTemplate(settings.MessageTemplate, map[string]string{
		"alias":        settings.Alias,
		"alias_prefix": formatBarkAliasPrefix(settings.Alias),
		"number":       number,
		"started_at":   call.StartedAt.Local().Format("2006-01-02 15:04:05"),
		"ended_at":     endedAt,
		"duration":     duration,
		"status":       status,
	})
}

func formatCallDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	seconds := int(duration.Round(time.Second).Seconds())
	hours := seconds / 3600
	minutes := seconds % 3600 / 60
	seconds %= 60
	if hours > 0 {
		return fmt.Sprintf("%d小时%d分%d秒", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%d分%d秒", minutes, seconds)
	}
	return fmt.Sprintf("%d秒", seconds)
}

func buildBarkURL(settings barkChannelSettings, content string) (string, error) {
	if err := validateBarkURL(strings.TrimSpace(settings.APIURL), settings.Enabled, "通知"); err != nil {
		return "", err
	}
	return strings.Replace(settings.APIURL, barkMessagePlaceholder, url.PathEscape(content), 1), nil
}

func sendBarkNotification(ctx context.Context, client *http.Client, settings barkChannelSettings, content string) error {
	target, err := buildBarkURL(settings, content)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errors.New("创建 Bark 通知请求失败")
	}
	request.Header.Set("User-Agent", "DJOneHub/Bark")
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Bark API 请求失败")
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Bark API 返回 HTTP %d", response.StatusCode)
	}
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &result) == nil && result.Code != 0 && result.Code != http.StatusOK {
		if strings.TrimSpace(result.Message) != "" {
			return fmt.Errorf("Bark API 返回错误：%s", strings.TrimSpace(result.Message))
		}
		return fmt.Errorf("Bark API 返回错误码 %d", result.Code)
	}
	return nil
}

func (a *app) loadBarkSettingsLocked() error {
	if a.barkSettingsLoaded {
		return nil
	}
	if err := a.ensureBarkSettingsPathLocked(); err != nil {
		return err
	}
	settings := barkSettings{}
	data, err := os.ReadFile(a.barkSettingsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取 Bark 配置失败: %w", err)
	}
	if len(data) > 0 {
		var document barkSettingsDocument
		if err := json.Unmarshal(data, &document); err != nil {
			return fmt.Errorf("解析 Bark 配置失败: %w", err)
		}
		if document.Version >= barkConfigVersion || document.SMS != nil || document.MissedCall != nil {
			settings.Version = barkConfigVersion
			if document.SMS != nil {
				settings.SMS = *document.SMS
			}
			if document.MissedCall != nil {
				settings.MissedCall = *document.MissedCall
			}
		} else {
			settings.SMS = barkChannelSettings{
				Enabled: document.Enabled,
				APIURL:  document.APIURL,
				Alias:   document.Alias,
			}
		}
	}
	settings = normalizeBarkSettings(settings)
	if err := validateBarkSettings(settings); err != nil {
		return fmt.Errorf("Bark 配置无效: %w", err)
	}
	a.barkSettings = settings
	a.barkSettingsLoaded = true
	return nil
}

func (a *app) ensureBarkSettingsPathLocked() error {
	if a.barkSettingsPath != "" {
		return nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("定位 Bark 配置目录失败: %w", err)
	}
	a.barkSettingsPath = filepath.Join(configDir, "DJOneHub", "bark-settings.json")
	return nil
}

func (a *app) persistBarkSettingsLocked() error {
	if err := a.ensureBarkSettingsPathLocked(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.barkSettingsPath), 0o700); err != nil {
		return fmt.Errorf("创建 Bark 配置目录失败: %w", err)
	}
	data, err := json.MarshalIndent(a.barkSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 Bark 配置失败: %w", err)
	}
	temporary := a.barkSettingsPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("写入 Bark 配置失败: %w", err)
	}
	if err := os.Rename(temporary, a.barkSettingsPath); err != nil {
		return fmt.Errorf("替换 Bark 配置失败: %w", err)
	}
	return nil
}

func (a *app) currentBarkSettings() (barkSettings, error) {
	a.barkMu.Lock()
	defer a.barkMu.Unlock()
	if err := a.loadBarkSettingsLocked(); err != nil {
		return barkSettings{}, err
	}
	return a.barkSettings, nil
}

func (a *app) forwardSMSBark(message receivedSMS) {
	if strings.HasPrefix(message.Sender, "已发送至 ") {
		return
	}
	settings, err := a.currentBarkSettings()
	if err != nil {
		log.Printf("Bark SMS forwarding skipped: %v", err)
		return
	}
	channel := settings.SMS
	if !channel.Enabled {
		return
	}
	content := renderSMSBarkMessage(channel, message)
	go a.sendBarkAsync(channel, content, "SMS")
}

func (a *app) forwardCallBark(call callRecord) {
	if !call.Missed && !call.AIHandled {
		return
	}
	settings, err := a.currentBarkSettings()
	if err != nil {
		log.Printf("Bark call forwarding skipped: %v", err)
		return
	}
	channel := settings.MissedCall
	if !channel.Enabled {
		return
	}
	content := renderCallBarkMessage(channel, call)
	go a.sendBarkAsync(channel, content, "call")
}

func (a *app) sendBarkAsync(settings barkChannelSettings, content, label string) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := sendBarkNotification(ctx, a.barkHTTPClient, settings, content); err != nil {
		log.Printf("Bark %s forwarding failed: %v", label, err)
		return
	}
	log.Printf("%s forwarded to Bark successfully", label)
}

func selectBarkChannel(settings barkSettings, kind barkChannelKind) barkChannelSettings {
	if kind == barkChannelMissedCall {
		return settings.MissedCall
	}
	return settings.SMS
}

func setBarkChannel(settings *barkSettings, kind barkChannelKind, channel barkChannelSettings) {
	if kind == barkChannelMissedCall {
		settings.MissedCall = channel
		return
	}
	settings.SMS = channel
}

func (a *app) getBarkChannelSettings(w http.ResponseWriter, kind barkChannelKind) {
	w.Header().Set("Cache-Control", "no-store")
	settings, err := a.currentBarkSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, selectBarkChannel(settings, kind))
}

func (a *app) saveBarkChannelSettings(w http.ResponseWriter, r *http.Request, kind barkChannelKind) {
	w.Header().Set("Cache-Control", "no-store")
	var channel barkChannelSettings
	if !decodeJSON(w, r, &channel) {
		return
	}
	channel = normalizeBarkChannelSettings(channel, barkChannelDefaultTemplate(kind))
	if err := validateBarkChannelSettings(kind, channel); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	a.barkMu.Lock()
	defer a.barkMu.Unlock()
	if err := a.loadBarkSettingsLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	previous := a.barkSettings
	setBarkChannel(&a.barkSettings, kind, channel)
	a.barkSettings.Version = barkConfigVersion
	if err := a.persistBarkSettingsLocked(); err != nil {
		a.barkSettings = previous
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":  barkChannelLabel(kind) + " Bark 通知配置已保存",
		"settings": channel,
	})
}

func (a *app) testBarkChannelSettings(w http.ResponseWriter, kind barkChannelKind) {
	w.Header().Set("Cache-Control", "no-store")
	settings, err := a.currentBarkSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	channel := selectBarkChannel(settings, kind)
	if channel.APIURL == "" {
		writeError(w, http.StatusBadRequest, "请先保存 "+barkChannelLabel(kind)+" Bark API 链接")
		return
	}
	now := time.Now()
	content := renderSMSBarkMessage(channel, receivedSMS{
		Sender: "10086", Content: "DJOneHub Bark 短信通知测试", Code: "123456", Timestamp: now,
	})
	if kind == barkChannelMissedCall {
		ended := now
		content = renderCallBarkMessage(channel, callRecord{
			Number: "13800138000", StartedAt: now.Add(-8 * time.Second), EndedAt: &ended, Missed: true,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := sendBarkNotification(ctx, a.barkHTTPClient, channel, content); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": barkChannelLabel(kind) + " Bark 测试通知已发送"})
}

func (a *app) getSMSBarkSettings(w http.ResponseWriter, _ *http.Request) {
	a.getBarkChannelSettings(w, barkChannelSMS)
}

func (a *app) saveSMSBarkSettings(w http.ResponseWriter, r *http.Request) {
	a.saveBarkChannelSettings(w, r, barkChannelSMS)
}

func (a *app) testSMSBarkSettings(w http.ResponseWriter, _ *http.Request) {
	a.testBarkChannelSettings(w, barkChannelSMS)
}

func (a *app) getMissedCallBarkSettings(w http.ResponseWriter, _ *http.Request) {
	a.getBarkChannelSettings(w, barkChannelMissedCall)
}

func (a *app) saveMissedCallBarkSettings(w http.ResponseWriter, r *http.Request) {
	a.saveBarkChannelSettings(w, r, barkChannelMissedCall)
}

func (a *app) testMissedCallBarkSettings(w http.ResponseWriter, _ *http.Request) {
	a.testBarkChannelSettings(w, barkChannelMissedCall)
}
