package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestStartPlanIsNotAdvertisedOrManaged(t *testing.T) {
	for _, field := range pluginRegistration().Metadata.ConfigFields {
		if field.Name == "start_plan_enabled" {
			t.Fatal("start plan must not be advertised as a configurable management capability")
		}
	}

	body := url.Values{
		"action":     {"start_plan"},
		"auth_index": {"account"},
		"plan_id":    {"plan"},
		"confirm":    {"true"},
	}.Encode()
	raw, err := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Body:   []byte(body),
	}})
	if err != nil {
		t.Fatal(err)
	}
	responseRaw, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(responseRaw, &env); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusNotImplemented {
		t.Fatal("start plan must not return a 501 placeholder that implies a management capability")
	}
	if response.StatusCode < http.StatusBadRequest {
		t.Fatalf("start plan management action unexpectedly accepted: status=%d", response.StatusCode)
	}
}
