package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	traeAgentHost         = "https://trae-api-cn.mchost.guru"
	traeUGHost            = "https://api.trae.cn"
	traeChatPath          = "/api/agent/v3/llm_utils_chat"
	traeModelsPath        = "/api/ide/v1/get_detail_param"
	traeCheckinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	traeCheckinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"
	traeCreditsPath       = "/trae/api/v2/pay/ide_user_ent_usage"
	maxUpstreamBody       = 16 << 20
)

var hostHTTPCall = func(callback string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return executeHTTP(context.Background(), nil, callback, req)
}

type hostHTTPDoer struct{ callback string }

func (d hostHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(io.LimitReader(req.Body, maxUpstreamBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxUpstreamBody {
		return nil, errors.New("request too large")
	}
	resp, err := hostHTTPCall(d.callback, pluginapi.HTTPRequest{Method: req.Method, URL: req.URL.String(), Headers: req.Header.Clone(), Body: body})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: resp.StatusCode, Header: resp.Headers.Clone(), Body: io.NopCloser(bytes.NewReader(resp.Body))}, nil
}

func traeHeaders(s *authStorage, stream bool) http.Header {
	h := http.Header{"Content-Type": {"application/json"}, "User-Agent": {"Trae/" + traeIDEVersion}, "Authorization": {"Cloud-IDE-JWT " + s.AccessToken}, "X-Cloudide-Token": {s.AccessToken}, "X-Ide-Token": {s.AccessToken}, "X-App-Id": {"6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"}, "X-App-Version": {"default"}, "X-Ide-Version": {traeIDEVersion}, "X-Ide-Version-Code": {"20260716"}, "X-App-Version-Code": {"20260716"}, "X-Ide-Version-Type": {"stable"}, "X-Device-Type": {"windows"}, "X-OS-Version": {"Windows 11 Pro"}, "X-Device-Brand": {"83DG"}, "Request-Traffic-Type": {"prod"}}
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	if s.UID != "" {
		h.Set("X-Uid", s.UID)
	}
	if s.MachineID != "" {
		h.Set("X-Machine-Id", s.MachineID)
	}
	if s.DeviceID != "" {
		h.Set("X-Device-Id", s.DeviceID)
	}
	return h
}
func ugHeaders(s *authStorage) http.Header {
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}, "User-Agent": {"Trae/" + traeIDEVersion}, "Authorization": {"Cloud-IDE-JWT " + s.AccessToken}, "X-User-Region": {"CN"}}
	if s.DeviceID != "" {
		h.Set("X-Device-Id", s.DeviceID)
	}
	return h
}

func boundedDo(ctx context.Context, callback string, req pluginapi.HTTPRequest, limit int) (pluginapi.HTTPResponse, error) {
	if err := validateFixedURL(req.URL); err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	resp, err := hostHTTPCall(callback, req)
	if err != nil {
		return resp, err
	}
	if len(resp.Body) > limit {
		return pluginapi.HTTPResponse{}, errors.New("upstream response too large")
	}
	return resp, nil
}
func validateFixedURL(raw string) error {
	for _, v := range []string{traeAgentHost, traeUGHost, traeAPIHost} {
		if strings.HasPrefix(raw, v+"/") {
			return nil
		}
	}
	return errors.New("refusing credentialed request to untrusted host")
}
func upstreamStatus(resp pluginapi.HTTPResponse) error {
	if resp.StatusCode < 400 {
		return nil
	}
	return fmt.Errorf("traework %s (http %d)", classifyTraeStatus(resp.StatusCode, resp.Body), resp.StatusCode)
}
func classifyTraeStatus(status int, body []byte) string {
	s := strings.ToLower(string(body))
	if strings.Contains(s, "1005") && strings.Contains(s, "plan") {
		return "plan_limit"
	}
	switch {
	case status == 401 || status == 403:
		return "credential_invalid"
	case status == 429:
		return "rate_limited"
	case status == 404:
		return "not_found"
	case status >= 500:
		return "upstream_unavailable"
	default:
		return "bad_request"
	}
}

type traeAccountClient struct {
	auth     HostAuth
	callback string
}

func (c traeAccountClient) storage(ctx context.Context, a ScheduledAccount) (*authStorage, error) {
	d, e := c.auth.Get(ctx, a.AuthIndex)
	if e != nil {
		return nil, e
	}
	return parseAuthStorage(d.JSON)
}
func (c traeAccountClient) call(ctx context.Context, a ScheduledAccount, path string) ([]byte, error) {
	s, e := c.storage(ctx, a)
	if e != nil {
		return nil, e
	}
	r, e := boundedDo(ctx, c.callback, pluginapi.HTTPRequest{Method: "POST", URL: traeUGHost + path, Headers: ugHeaders(s), Body: []byte("{}")}, 1<<20)
	if e != nil {
		return nil, e
	}
	if e = upstreamStatus(r); e != nil {
		return nil, e
	}
	return r.Body, nil
}
func (c traeAccountClient) CheckinStatus(ctx context.Context, a ScheduledAccount) (bool, int64, bool, error) {
	b, e := c.call(ctx, a, traeCheckinStatusPath)
	if e != nil {
		return false, 0, false, e
	}
	var v struct {
		Checked bool  `json:"checked_in"`
		Credits int64 `json:"credits"`
		Enable  bool  `json:"enable"`
	}
	e = json.Unmarshal(b, &v)
	return v.Checked, v.Credits, v.Enable, e
}
func (c traeAccountClient) CheckinClaim(ctx context.Context, a ScheduledAccount) error {
	b, e := c.call(ctx, a, traeCheckinClaimPath)
	if e != nil {
		return e
	}
	var v struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return e
	}
	if v.Code != 0 {
		return fmt.Errorf("checkin claim failed: code=%d msg=%s", v.Code, v.Message)
	}
	return nil
}
func (c traeAccountClient) Credits(ctx context.Context, a ScheduledAccount) (int64, error) {
	b, e := c.call(ctx, a, traeCreditsPath)
	if e != nil {
		return 0, e
	}
	var v struct {
		Packs []struct {
			Base struct {
				Quota struct {
					Limit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
		} `json:"user_entitlement_pack_list"`
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return 0, e
	}
	var n int64
	for _, p := range v.Packs {
		n += p.Base.Quota.Limit
	}
	return n, nil
}

var modelCache struct {
	sync.Mutex
	at     time.Time
	models []pluginapi.ModelInfo
}
