package launcherpredownload

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
)

// 鹰角 PC 客户端把本地配置以 AES-256-CBC（PKCS#7）落盘。
//
// 这两个常量是社区逆向得到的公开值（多个独立来源一致），只用于**读取本机
// 已安装的游戏版本与区服信息**，不参与任何写入。
//
// 维护提示：如果哪天启动器换了密钥，decrypt 会返回错误，调用方降级为
// "读不到版本 → 跳过本次检查"，不会影响任务运行；届时更新这里的常量即可。
const (
	gameConfigKeyHex = "c0f30e1ce763bbc21cc355a34303ac50399444bff68c4a22af398c0a166ee143"
	gameConfigIVHex  = "33467861192750649501937264608400"
)

const (
	gameConfigFile   = "config.ini"
	gameExtraConfig  = "U8Data/config/u8ExtraConfig.bin"
	extraConfigField = "launcherUrl"
)

// gameConfig 是从游戏安装目录读到的、调用官方接口所需的全部信息。
type gameConfig struct {
	Dir         string // 游戏安装目录：更新完成后要重新读它来确认版本
	Version     string
	AppCode     string
	Region      string
	Channel     string
	SubChannel  string
	LauncherURL string
}

// readGameConfig 解密并解析游戏安装目录下的 config.ini 与 u8ExtraConfig.bin。
//
// 这两个文件共同给出了接口基址、appcode、区服、渠道与已安装版本，因此本包
// **不需要硬编码任何厂商常量**，国服与国际服走同一套代码。
func readGameConfig(gameDir string) (*gameConfig, error) {
	iniPlain, err := readDecrypted(filepath.Join(gameDir, gameConfigFile))
	if err != nil {
		return nil, err
	}

	values := parseINI(string(iniPlain))
	cfg := &gameConfig{
		Dir:        gameDir,
		Version:    values["Game/version"],
		AppCode:    values["Game/appcode"],
		Region:     values["Game/region"],
		Channel:    values["Game/channel"],
		SubChannel: values["Game/sub_channel"],
	}

	if cfg.Version == "" || cfg.AppCode == "" {
		return nil, fmt.Errorf("launcherpredownload: %s has no version/appcode", gameConfigFile)
	}

	// 官方接口在渠道缺省时用 1，实测国服 channel=sub_channel=1。
	if cfg.Channel == "" {
		cfg.Channel = "1"
	}
	if cfg.SubChannel == "" {
		cfg.SubChannel = cfg.Channel
	}

	extraPlain, err := readDecrypted(filepath.Join(gameDir, filepath.FromSlash(gameExtraConfig)))
	if err != nil {
		return nil, err
	}

	var extra struct {
		LauncherURL string `json:"launcherUrl"`
		AppCode     string `json:"appCode"`
	}
	if err := json.Unmarshal(extraPlain, &extra); err != nil {
		return nil, fmt.Errorf("launcherpredownload: parse u8ExtraConfig.bin: %w", err)
	}

	cfg.LauncherURL = strings.TrimRight(strings.TrimSpace(extra.LauncherURL), "/")
	if cfg.LauncherURL == "" {
		return nil, fmt.Errorf("launcherpredownload: %s has no %s", gameExtraConfig, extraConfigField)
	}

	if extra.AppCode != "" && extra.AppCode != cfg.AppCode {
		log.Warn().
			Str("component", componentName).
			Str("config_ini_appcode", cfg.AppCode).
			Str("extra_config_appcode", extra.AppCode).
			Msg("appcode differs between local config files, trusting config.ini")
	}

	return cfg, nil
}

// readDecrypted 读取文件并做 AES-256-CBC 解密。
func readDecrypted(path string) ([]byte, error) {
	sealed, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("launcherpredownload: read %s: %w", path, err)
	}

	plain, err := decrypt(sealed)
	if err != nil {
		return nil, fmt.Errorf("launcherpredownload: decrypt %s: %w", path, err)
	}

	return plain, nil
}

// decrypt 做 AES-256-CBC 解密并去除 PKCS#7 填充。
func decrypt(sealed []byte) ([]byte, error) {
	key, err := hex.DecodeString(gameConfigKeyHex)
	if err != nil {
		return nil, fmt.Errorf("launcherpredownload: invalid key constant: %w", err)
	}

	iv, err := hex.DecodeString(gameConfigIVHex)
	if err != nil {
		return nil, fmt.Errorf("launcherpredownload: invalid iv constant: %w", err)
	}

	if len(sealed) == 0 || len(sealed)%aes.BlockSize != 0 {
		return nil, fmt.Errorf(
			"launcherpredownload: ciphertext length %d is not a positive multiple of %d",
			len(sealed), aes.BlockSize,
		)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("launcherpredownload: new cipher: %w", err)
	}

	plain := make([]byte, len(sealed))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, sealed)

	return pkcs7Unpad(plain)
}

// pkcs7Unpad 去除 PKCS#7 填充；填充非法时返回错误，交由调用方降级。
func pkcs7Unpad(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, errors.New("launcherpredownload: empty plaintext")
	}

	padding := int(plain[len(plain)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(plain) {
		return nil, fmt.Errorf("launcherpredownload: invalid PKCS#7 padding length %d", padding)
	}

	for _, b := range plain[len(plain)-padding:] {
		if int(b) != padding {
			return nil, errors.New("launcherpredownload: invalid PKCS#7 padding bytes")
		}
	}

	return plain[:len(plain)-padding], nil
}

// parseINI 解析游戏 config.ini 用到的极简语法：`[section]` 段落、`key=value`、
// 以 # 或 ; 开头的注释。返回 "section/key" 到值的映射。
func parseINI(text string) map[string]string {
	values := make(map[string]string)
	section := ""

	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		values[section+"/"+strings.TrimSpace(key)] = strings.TrimSpace(value)
	}

	return values
}
