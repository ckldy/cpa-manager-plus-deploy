// callback.go turns a pasted TRAE callback URL into a persistable AuthData.
//
// TRAE restricts auth_callback_url to the loopback endpoint its own IDE listens
// on (http://127.0.0.1:18080/authorize). Verified 2026-08-31 by A/B loading the
// authorization page: the localhost callback renders the login form, while any
// public host — https or http, any port or path — fails with "登录失败 / 网络
// 错误". The credential therefore can never be delivered to a remote CPA by the
// browser; the admin copies the callback URL out of the address bar and pastes
// it into the plugin panel.
package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// completeLoginExchange trades callback material for a usable token pair and
// builds the auth record the host persists.
func completeLoginExchange(client HTTPDoer, session loginSession, parsed loginCallback) (pluginapi.AuthData, error) {
	token := firstNonEmpty(parsed.RefreshToken, parsed.UserJWT.RefreshToken)
	ex, err := exchangeTokenAt(context.Background(), client, traeAPIHost, traeClientID, token, time.Now())
	if err != nil {
		return pluginapi.AuthData{}, err
	}
	ui, err := GetUserInfo(context.Background(), client, traeAPIHost, ex.AccessToken)
	if err != nil {
		return pluginapi.AuthData{}, err
	}
	st := authStorage{AccessToken: ex.AccessToken, RefreshToken: ex.RefreshToken, ExpiresAt: ex.ExpiresAt, MachineID: session.MachineID, DeviceID: session.DeviceID, UID: ui.UserID, EnterpriseID: ui.EnterpriseID, Nickname: ui.Nickname, Provider: ProviderTraeWork, Source: "oauth"}
	b, _ := json.Marshal(st)
	return pluginapi.AuthData{Provider: ProviderTraeWork, FileName: "traework-" + ui.UserID + ".json", Label: authLabel(&st), StorageJSON: b, Metadata: authMetadata(&st)}, nil
}
