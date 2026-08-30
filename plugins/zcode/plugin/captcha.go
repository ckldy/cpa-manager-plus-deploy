package main

import (
	"context"
	"errors"
	"sync"
)

// CaptchaProvider returns a fresh Aliyun verify parameter and its region.
// Implementations must never log the parameter because it is credential-bound.
type CaptchaProvider interface {
	Get(context.Context) (param, region string, err error)
	Invalidate()
}

type unavailableCaptchaProvider struct{}

func (unavailableCaptchaProvider) Get(context.Context) (string, string, error) {
	return "", "", errors.New("Coding Plan requires captcha verification, but no captcha provider is configured")
}

func (unavailableCaptchaProvider) Invalidate() {}

var (
	captchaProviderMu sync.RWMutex
	captchaProvider   CaptchaProvider = unavailableCaptchaProvider{}
)

func currentCaptchaProvider() CaptchaProvider {
	captchaProviderMu.RLock()
	defer captchaProviderMu.RUnlock()
	return captchaProvider
}

func setCaptchaProviderForTest(provider CaptchaProvider) func() {
	captchaProviderMu.Lock()
	previous := captchaProvider
	captchaProvider = provider
	captchaProviderMu.Unlock()
	return func() {
		captchaProviderMu.Lock()
		captchaProvider = previous
		captchaProviderMu.Unlock()
	}
}
