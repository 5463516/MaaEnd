package launcherpredownload

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// apiPath 是鹰角启动器自己使用的"取最新游戏信息"接口。
	apiPath = "/api/game/get_latest"

	// requestTimeout 取一个很短的值：这是任务开始前的检查，宁可跳过也不能卡住用户。
	requestTimeout = 3 * time.Second

	// maxResponseBytes 兜住异常响应，正常响应约 10 KB。
	maxResponseBytes = 1 << 20

	// action 字段的实测语义：0 = 已是最新，1 = 需要更新。
	actionUpToDate       = 0
	actionUpdateRequired = 1
)

// prePatch 是响应里的预下载数据；官方未公开该字段，缺失或改名都按"没有预下载"处理。
type prePatch struct {
	Version     string `json:"version"`
	PackageSize string `json:"package_size"`
	TotalSize   string `json:"total_size"`
}

// latestGame 对应 /api/game/get_latest 的响应，只声明本包用得到的字段。
type latestGame struct {
	Action        int       `json:"action"`
	Version       string    `json:"version"`
	ClientVersion string    `json:"client_version"`
	PrePatch      *prePatch `json:"pre_patch"`
}

// PreDownloadVersion 返回预下载的目标版本；没有预下载时返回空串。
func (l *latestGame) PreDownloadVersion() string {
	if l == nil || l.PrePatch == nil {
		return ""
	}

	return strings.TrimSpace(l.PrePatch.Version)
}

// PreDownloadPackageSize 返回预下载包的字节数（字符串）；没有时返回空串。
func (l *latestGame) PreDownloadPackageSize() string {
	if l == nil || l.PrePatch == nil {
		return ""
	}

	return strings.TrimSpace(l.PrePatch.PackageSize)
}

// fetchLatest 查询官方接口。
//
// 只发送 appcode / channel / sub_channel / version 四个公开参数：
// 不登录、不带账号、不带设备标识，也不下载任何游戏数据。
func fetchLatest(cfg *gameConfig) (*latestGame, error) {
	endpoint, err := url.Parse(cfg.LauncherURL + apiPath)
	if err != nil {
		return nil, fmt.Errorf("launcherpredownload: invalid launcher url %q: %w", cfg.LauncherURL, err)
	}

	query := endpoint.Query()
	query.Set("appcode", cfg.AppCode)
	query.Set("channel", cfg.Channel)
	query.Set("sub_channel", cfg.SubChannel)
	query.Set("version", cfg.Version)
	endpoint.RawQuery = query.Encode()

	client := &http.Client{Timeout: requestTimeout}

	resp, err := client.Get(endpoint.String())
	if err != nil {
		return nil, fmt.Errorf("launcherpredownload: request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("launcherpredownload: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("launcherpredownload: read response: %w", err)
	}

	var latest latestGame
	if err := json.Unmarshal(body, &latest); err != nil {
		return nil, fmt.Errorf("launcherpredownload: parse response: %w", err)
	}

	return &latest, nil
}
