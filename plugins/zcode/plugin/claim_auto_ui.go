package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleAddCaptcha adds a user-supplied Aliyun captcha token to the in-memory
// pool. The token is never persisted, logged, or returned; the response only
// carries the new ready count and a sanitized message.
func handleAddCaptcha(values url.Values) pluginapi.ManagementResponse {
	if len(values) < 2 || len(values) > 3 || len(values["action"]) != 1 || len(values["param"]) != 1 || values.Get("action") != "add_captcha" {
		return claimManagementResponse(http.StatusBadRequest, "提交字段无效")
	}
	if region, exists := values["region"]; exists && len(region) != 1 {
		return claimManagementResponse(http.StatusBadRequest, "提交字段无效")
	}
	param := strings.TrimSpace(values.Get("param"))
	region := strings.TrimSpace(values.Get("region"))
	if param == "" {
		return claimManagementResponse(http.StatusBadRequest, "缺少验证码参数")
	}
	if len(param) < captchaPoolParamMin || len(param) > captchaPoolParamMax {
		return claimManagementResponse(http.StatusBadRequest, "验证码参数长度无效")
	}
	if len(region) > captchaPoolRegionMax {
		return claimManagementResponse(http.StatusBadRequest, "验证码区域长度无效")
	}
	if err := claimCaptchaPool.add(param, region, time.Now()); err != nil {
		return claimManagementResponse(http.StatusConflict, err.Error())
	}
	ready := claimCaptchaPool.stats(time.Now())
	body, _ := json.Marshal(map[string]any{"ok": true, "message": "验证码已加入池", "ready": ready})
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: body}
}

// handleClearCaptcha empties the in-memory captcha pool (admin action).
func handleClearCaptcha(values url.Values) pluginapi.ManagementResponse {
	if len(values) != 1 || len(values["action"]) != 1 || values.Get("action") != "clear_captcha" {
		return claimManagementResponse(http.StatusBadRequest, "提交字段无效")
	}
	claimCaptchaPool.clear()
	body, _ := json.Marshal(map[string]any{"ok": true, "message": "验证码池已清空", "ready": 0})
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: body}
}

// renderClaimAutoPanel shows the start-plan auto-claim state and the captcha
// pool management form. It never renders captcha tokens, only counts.
func renderClaimAutoPanel(now time.Time) []byte {
	status := claimAutoStatus(now)
	enabled := status["enabled"].(bool)
	running := status["running"].(bool)
	stopped := status["stopped"].(bool)
	held := status["held_accounts"].(int)
	ready := status["captcha_ready"].(int)
	planID := status["plan_id"].(string)
	pollMs := status["poll_interval_ms"].(int)
	cooldownMs := status["cooldown_ms"].(int)

	stateText, stateClass := "已关闭", "muted"
	if enabled {
		if stopped {
			stateText, stateClass = "已停止（需重新登录或重新启用）", "bad"
		} else if running {
			stateText, stateClass = "运行中", "good"
		} else {
			stateText, stateClass = "等待配置回传生效", "warn"
		}
	}
	planText := "最高优先级"
	if planID != "" {
		planText = planID
	}
	poolText := "空"
	if ready > 0 {
		poolText = "有可用 token"
	}
	solverStatus := captchaSolverStatus()
	solverText, solverDetail := "未启用", "使用手动粘贴 token"
	if enabled, _ := solverStatus["enabled"].(bool); enabled {
		solverText = "运行中"
		solverDetail = fmt.Sprintf("已求解 %d · 失败 %d · 池 %d", solverStatus["solves"].(int), solverStatus["failures"].(int), solverStatus["pool_ready"].(int))
		if lastErr, _ := solverStatus["last_error"].(string); lastErr != "" {
			solverDetail = "最近失败: " + lastErr
		}
	}

	return []byte(fmt.Sprintf(`<section class="section claim-auto-panel"><div class="section-head"><div><h2>Start Plan 自动领取</h2><p class="section-note">默认关闭。启用会轮询官方周末/体验套餐预览并对 JWT 账号自动领取，每次消费一个验证码池 token。自动求解（captcha_solver_enabled）从本机 Node solver 服务预热验证码池；否则 token 由你在官方 ZCode 验证会话取得并粘贴到下方池中，内存单次使用、不持久化。登录失效会停止调度，需重新启用。</p></div><span class="state %s">%s</span></div><div class="diagnostic-grid"><article><span>调度状态</span><b>%s</b><small>持有中账号 %d</small></article><article><span>领取目标</span><b>%s</b><small>轮询 %d s · 冷却 %d s</small></article><article><span>验证码池</span><b>%d</b><small>%s · 单次使用</small></article><article><span>自动求解</span><b>%s</b><small>%s</small></article></div><div class="captcha-manage"><h3>验证码池</h3><p>从官方 ZCode 验证会话复制 <code>X-Aliyun-Captcha-Verify-Param</code>（可选 <code>X-Aliyun-Captcha-Verify-Region</code>）粘贴到池中，供自动领取消费。参数不会被保存或显示。</p><form id="captcha-form" method="post" class="captcha-form"><input type="hidden" name="action" value="add_captcha"><input type="text" name="param" required maxlength="4096" placeholder="captcha_verify_param" autocomplete="off"><input type="text" name="region" maxlength="64" placeholder="region（可选）" autocomplete="off"><button class="action primary" type="submit">加入池</button><span id="captcha-result" role="status"></span></form><button type="button" class="action" id="captcha-clear">清空池</button><span id="captcha-clear-result" role="status"></span></div></section>`, stateClass, stateText, stateText, held, html.EscapeString(planText), pollMs/1000, cooldownMs/1000, ready, poolText, solverText, solverDetail))
}

// The JS is appended by renderClaimAutoScript into the management page so it
// can be injected alongside the panel markup.
func renderClaimAutoScript() string {
	return `<script>document.getElementById('captcha-form').addEventListener('submit',async function(event){event.preventDefault();var form=event.currentTarget,button=form.querySelector('button[type=submit]'),result=document.getElementById('captcha-result'),clear=document.getElementById('captcha-clear-result');button.disabled=true;result.className='';result.textContent='正在加入…';try{var response=await fetch(location.pathname,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},credentials:'same-origin',cache:'no-store',body:new URLSearchParams(new FormData(form)).toString()}),body=await response.json();result.textContent=body.message||('加入失败（HTTP '+response.status+')');if(response.ok&&body.ok){form.querySelector('input[name=param]').value='';form.querySelector('input[name=region]').value='';result.className='success-text';document.getElementById('captcha-count')&&(document.getElementById('captcha-count').textContent=body.ready)}else{result.className='error-text'}}catch(error){result.textContent='加入失败：'+error.message}finally{button.disabled=false}});document.getElementById('captcha-clear').addEventListener('click',async function(){var button=this,result=document.getElementById('captcha-clear-result');button.disabled=true;result.className='';result.textContent='正在清空…';try{var response=await fetch(location.pathname,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},credentials:'same-origin',cache:'no-store',body:'action=clear_captcha'}),body=await response.json();result.textContent=body.message||('清空失败（HTTP '+response.status+')');if(response.ok&&body.ok)result.className='success-text';else result.className='error-text'}catch(error){result.textContent='清空失败：'+error.message}finally{button.disabled=false}})</script>`
}
