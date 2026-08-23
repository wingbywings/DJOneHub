package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	upstreamVoiceRuntimeCommit  = "0443dfdaf8aec086fd76ba2ee9152fd908114524"
	upstreamVoiceRuntimeSource  = "moluncn/mavo@" + upstreamVoiceRuntimeCommit
	upstreamVoiceRuntimeBase    = "https://raw.githubusercontent.com/moluncn/mavo/" + upstreamVoiceRuntimeCommit + "/Resources/ModuleVoice/"
	upstreamVoiceRuntimeAPIBase = "https://api.github.com/repos/moluncn/mavo/contents/Resources/ModuleVoice/"
)

type voiceRuntimeManifest struct {
	RuntimeVersion  string
	KernelRelease   string
	CardName        string
	Helper          string
	Files           []voiceRuntimeManifestFile
	Modules         []voiceRuntimeModule
	RequiredDevices []string
}

type voiceRuntimeManifestFile struct {
	Name string
	Mode uint32
}

type voiceRuntimeModule struct {
	File string
	Name string
}

type upstreamVoiceFile struct {
	Name   string
	Mode   os.FileMode
	SHA256 string
}

type upstreamVoiceDownloadSource struct {
	Name string
	URL  string
}

var upstreamVoiceFiles = []upstreamVoiceFile{
	{Name: "qdc507_aprv3.ko", Mode: 0o644, SHA256: "3d82d3dec4f1e323201bba87156df9d41438e08314097353f2607f9117211d4a"},
	{Name: "qdc507_voice.ko", Mode: 0o644, SHA256: "ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c"},
	{Name: "mavo-pcm-bridge.armv7", Mode: 0o755, SHA256: "88d47c15e61d1428a59c821fed804c2e6490e82859a085062f21966b58d167fc"},
}

func loadVoiceManifest() voiceRuntimeManifest {
	return voiceRuntimeManifest{
		RuntimeVersion: "qdc507-3.18.44-voice-20260712.5",
		KernelRelease:  "3.18.44",
		CardName:       "mdm9607-tomtom-i2s-snd-card",
		Helper:         "mavo-pcm-bridge.armv7",
		Files: []voiceRuntimeManifestFile{
			{Name: "qdc507_aprv3.ko", Mode: 0o644},
			{Name: "qdc507_voice.ko", Mode: 0o644},
			{Name: "mavo-pcm-bridge.armv7", Mode: 0o755},
		},
		Modules: []voiceRuntimeModule{
			{File: "qdc507_aprv3.ko", Name: "qdc507_aprv3"},
			{File: "qdc507_voice.ko", Name: "qdc507_voice"},
		},
		RequiredDevices: []string{
			"/dev/snd/controlC0", "/dev/snd/pcmC0D4p", "/dev/snd/pcmC0D4c",
			"/dev/snd/pcmC0D5p", "/dev/snd/pcmC0D6c",
		},
	}
}

func upstreamVoiceRuntimeDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法确定语音运行时目录: %w", err)
	}
	return filepath.Join(base, "DJOneHub", "voice-runtime", "mavo-0443dfd"), nil
}

func upstreamVoiceRuntimeInstalled() (bool, string) {
	directory, err := upstreamVoiceRuntimeDir()
	if err != nil {
		return false, err.Error()
	}
	for _, file := range upstreamVoiceFiles {
		data, err := os.ReadFile(filepath.Join(directory, file.Name))
		if err != nil {
			return false, "尚未从上游安装语音运行时"
		}
		if !matchesSHA256(data, file.SHA256) {
			return false, "本地语音运行时校验失败，请重新安装"
		}
	}
	return true, "已从 " + upstreamVoiceRuntimeSource + " 校验安装"
}

func readUpstreamVoiceRuntimeFile(name string) ([]byte, error) {
	for _, file := range upstreamVoiceFiles {
		if file.Name != name {
			continue
		}
		directory, err := upstreamVoiceRuntimeDir()
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(directory, file.Name))
		if err != nil {
			return nil, errors.New("未安装模块侧语音运行时；请先明确确认安装")
		}
		if !matchesSHA256(data, file.SHA256) {
			return nil, fmt.Errorf("语音运行时 %s 校验失败，请重新安装", file.Name)
		}
		return data, nil
	}
	return nil, fmt.Errorf("未知语音运行时文件: %s", name)
}

func provisionUpstreamVoiceRuntime(ctx context.Context) error {
	if installed, _ := upstreamVoiceRuntimeInstalled(); installed {
		return nil
	}
	directory, err := upstreamVoiceRuntimeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("无法创建语音运行时目录: %w", err)
	}
	client := &http.Client{Timeout: 35 * time.Second}
	for _, file := range upstreamVoiceFiles {
		sources := []upstreamVoiceDownloadSource{
			{Name: "GitHub Raw", URL: upstreamVoiceRuntimeBase + file.Name},
			{Name: "GitHub API", URL: upstreamVoiceRuntimeAPIBase + file.Name + "?ref=" + upstreamVoiceRuntimeCommit},
			{Name: "GitHub Raw retry", URL: upstreamVoiceRuntimeBase + file.Name},
		}
		if err := downloadVerifiedVoiceFile(ctx, client, directory, file, sources); err != nil {
			return err
		}
	}
	return nil
}

func downloadVerifiedVoiceFile(ctx context.Context, client *http.Client, directory string, file upstreamVoiceFile, sources []upstreamVoiceDownloadSource) error {
	var failures []string
	for index, source := range sources {
		data, err := fetchVerifiedVoiceRuntimeFile(ctx, client, source, file)
		if err == nil {
			return writeVerifiedVoiceRuntimeFile(directory, file, data)
		}
		failures = append(failures, source.Name+": "+err.Error())
		if index+1 < len(sources) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(index+1) * time.Second):
			}
		}
	}
	return fmt.Errorf("上游下载 %s 失败（可重试）：%s", file.Name, strings.Join(failures, "；"))
}

func fetchVerifiedVoiceRuntimeFile(ctx context.Context, client *http.Client, source upstreamVoiceDownloadSource, file upstreamVoiceFile) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github.raw")
	request.Header.Set("User-Agent", "DJOneHub")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if response.ContentLength > 32<<20 {
		return nil, errors.New("文件超过安全大小限制")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 32<<20 || !matchesSHA256(data, file.SHA256) {
		return nil, errors.New("SHA-256 不匹配，已拒绝安装")
	}
	return data, nil
}

func writeVerifiedVoiceRuntimeFile(directory string, file upstreamVoiceFile, data []byte) error {
	if !matchesSHA256(data, file.SHA256) {
		return errors.New("SHA-256 不匹配，已拒绝写入")
	}
	temporary, err := os.CreateTemp(directory, "."+file.Name+"-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(file.Mode); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(directory, file.Name))
}

func matchesSHA256(data []byte, expected string) bool {
	actual := sha256.Sum256(data)
	return strings.EqualFold(hex.EncodeToString(actual[:]), expected)
}
