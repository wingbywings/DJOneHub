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
	"strings"
	"time"
	"unicode/utf8"
)

const barkMessagePlaceholder = "{message}"

type barkSettings struct {
	Enabled bool   `json:"enabled"`
	APIURL  string `json:"api_url"`
	Alias   string `json:"alias"`
}

func normalizeBarkSettings(settings barkSettings) barkSettings {
	settings.APIURL = strings.TrimSpace(settings.APIURL)
	settings.Alias = strings.TrimSpace(settings.Alias)
	return settings
}

func validateBarkSettings(settings barkSettings) error {
	settings = normalizeBarkSettings(settings)
	if settings.APIURL == "" {
		if settings.Enabled {
			return errors.New("启用 Bark 通知前请填写 API 链接")
		}
		return nil
	}
	if strings.Count(settings.APIURL, barkMessagePlaceholder) != 1 {
		return fmt.Errorf("Bark API 链接必须包含一个 %s 占位符", barkMessagePlaceholder)
	}
	if len(settings.APIURL) > 2048 {
		return errors.New("Bark API 链接过长")
	}
	if utf8.RuneCountInString(settings.Alias) > 80 {
		return errors.New("Bark 自定义别名不能超过 80 个字符")
	}
	placeholderAt := strings.Index(settings.APIURL, barkMessagePlaceholder)
	if separatorAt := strings.IndexAny(settings.APIURL, "?#"); separatorAt >= 0 && placeholderAt > separatorAt {
		return fmt.Errorf("%s 占位符必须位于 Bark API 的路径中", barkMessagePlaceholder)
	}

	parsed, err := url.Parse(strings.Replace(settings.APIURL, barkMessagePlaceholder, "test", 1))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("Bark API 链接必须是有效的 HTTP 或 HTTPS 地址")
	}
	return nil
}

func formatBarkMessage(alias, content string) string {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return content
	}
	if (strings.HasPrefix(alias, "[") && strings.HasSuffix(alias, "]")) ||
		(strings.HasPrefix(alias, "【") && strings.HasSuffix(alias, "】")) {
		return alias + content
	}
	return "[" + alias + "]" + content
}

func buildBarkURL(settings barkSettings, content string) (string, error) {
	settings = normalizeBarkSettings(settings)
	if err := validateBarkSettings(settings); err != nil {
		return "", err
	}
	message := formatBarkMessage(settings.Alias, content)
	return strings.Replace(settings.APIURL, barkMessagePlaceholder, url.PathEscape(message), 1), nil
}

func sendBarkNotification(ctx context.Context, client *http.Client, settings barkSettings, content string) error {
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
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("解析 Bark 配置失败: %w", err)
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
	if !settings.Enabled {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if err := sendBarkNotification(ctx, a.barkHTTPClient, settings, message.Content); err != nil {
			log.Printf("Bark SMS forwarding failed: %v", err)
			return
		}
		log.Printf("SMS forwarded to Bark successfully")
	}()
}

func (a *app) getBarkSettings(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	settings, err := a.currentBarkSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (a *app) saveBarkSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var settings barkSettings
	if !decodeJSON(w, r, &settings) {
		return
	}
	settings = normalizeBarkSettings(settings)
	if err := validateBarkSettings(settings); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	a.barkMu.Lock()
	defer a.barkMu.Unlock()
	if err := a.ensureBarkSettingsPathLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	previous := a.barkSettings
	a.barkSettings = settings
	if err := a.persistBarkSettingsLocked(); err != nil {
		a.barkSettings = previous
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.barkSettingsLoaded = true
	writeJSON(w, http.StatusOK, map[string]any{"message": "Bark 通知配置已保存", "settings": settings})
}

func (a *app) testBarkSettings(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	settings, err := a.currentBarkSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if settings.APIURL == "" {
		writeError(w, http.StatusBadRequest, "请先保存 Bark API 链接")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := sendBarkNotification(ctx, a.barkHTTPClient, settings, "DJOneHub Bark 通知测试"); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Bark 测试通知已发送"})
}
