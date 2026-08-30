package main

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ZCode 控制面身份头（identity.go）
//
// 官方 ZCode 桌面客户端在每次上游/控制面请求都会携带一套“身份头”，使代理在指纹
// 层与官方客户端一致。本文件提供纯构造 + 基于 authStorage 的装配；device_mid
// 缺失时自动生成进程内稳定的 UUIDv4 并复用（同一进程内保持一致）。
//
// 本文件只读复用 executor.go 的 zcodeHeaders / zcodeClientVersion；
// 不修改 auth.go / claim.go。身份头常量的取值与现有平台标识一致
// （platform=linux-x64，参考 util.go/claim.go 中的查询参数）。

const (
	identitySourceTitle    = "electron"
	identityRefererOrigin  = "https://zcode.z.ai/"
	identityPlatform       = "linux"
	identityArch           = "x64"
	identityReleaseChannel = "production"
	identityClientLanguage = "zh-CN"
	identityClientTimezone = "Asia/Shanghai"
	identityOSCategory     = "linux"
	identityOSVersion      = "6.1"
)

// isPrintableASCII reports whether s is non-empty and contains only printable
// ASCII (0x20-0x7e), mirroring the official client's header-value gate. It
// rejects header injection carried in user-controlled storage fields.
func isPrintableASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// buildControlPlaneHeaders returns the pure control-plane identity header set
// (no credentials). deviceMid is included only when non-empty and printable.
func buildControlPlaneHeaders(deviceMid string) http.Header {
	h := http.Header{}
	h.Set("HTTP-Referer", identityRefererOrigin)
	h.Set("User-Agent", "ZCode/"+zcodeClientVersion)
	h.Set("X-ZCode-App-Version", zcodeClientVersion)
	h.Set("X-Title", "Z Code@"+identitySourceTitle)
	h.Set("X-ZCode-Agent", "glm")
	h.Set("X-Platform", identityPlatform+"-"+identityArch)
	h.Set("X-Release-Channel", identityReleaseChannel)
	h.Set("X-Client-Language", identityClientLanguage)
	h.Set("X-Client-Timezone", identityClientTimezone)
	h.Set("X-Os-Category", identityOSCategory)
	h.Set("X-Os-Version", identityOSVersion)
	if mid := strings.TrimSpace(deviceMid); isPrintableASCII(mid) {
		h.Set("X-Device-Mid", mid)
	}
	return h
}

// buildZCodeIdentityHeaders layers the enhanced control-plane identity headers
// on top of zcodeHeaders (Authorization / captcha / anthropic-version are
// preserved) and returns the resolved device_mid (existing value, or the
// auto-generated one) so callers can persist it when they have a place to.
func buildZCodeIdentityHeaders(s authStorage) (http.Header, string) {
	mid := resolveDeviceMID(s)
	h := zcodeHeaders(s.ZCodeJWTToken, s.CaptchaVerifyParam, s.CaptchaVerifyRegion)
	for key, values := range buildControlPlaneHeaders(mid) {
		h[key] = values
	}
	return h, mid
}

// generatedDeviceMID is a per-process auto-generated device_mid: created once
// and reused forever, matching the official client's “generated once and
// reused” behaviour within a single process lifetime.
var generatedDeviceMID = struct {
	sync.Mutex
	value string
}{}

// resolveDeviceMID returns the persisted device_mid when present (and printable
// ASCII); otherwise it auto-generates a stable UUIDv4 and reuses it.
func resolveDeviceMID(s authStorage) string {
	if mid := strings.TrimSpace(s.DeviceMID); isPrintableASCII(mid) {
		return mid
	}
	generatedDeviceMID.Lock()
	defer generatedDeviceMID.Unlock()
	if generatedDeviceMID.value == "" {
		generatedDeviceMID.value = newDeviceMID()
	}
	return generatedDeviceMID.value
}

// newDeviceMID returns a random RFC 4122 version 4 UUID (lowercase hex).
func newDeviceMID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		for i := 0; i < 16; i++ {
			b[i] = byte(now >> (8 * uint(i)))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
