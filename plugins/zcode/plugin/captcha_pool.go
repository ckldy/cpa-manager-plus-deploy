package main

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// Captcha pool: a bounded, single-use store of Aliyun captcha tokens that the
// user pastes from official ZCode verification sessions. The start-plan
// auto-claimer consumes one token per claim attempt (FIFO). This plugin never
// generates, forges, replays, persists, or logs captcha tokens — the pool is
// in-memory only and every token is dropped after a single take or on expiry.
//
// Semantics mirror the zcode-api (zcode-proxy) pre-solved token pool, minus
// the automatic solver: here the pool is warmed by the operator instead of by
// an in-process Aliyun happy-dom solver.

const (
	captchaPoolDefaultMax = 256
	captchaPoolDefaultTTL = 5 * time.Minute
	captchaPoolParamMin   = 8
	captchaPoolParamMax   = 4096
	captchaPoolRegionMax  = 64
)

type captchaPoolToken struct {
	Param   string
	Region  string
	AddedAt time.Time
}

type captchaPool struct {
	sync.Mutex
	items []captchaPoolToken
	max   int
	ttl   time.Duration
}

var claimCaptchaPool = &captchaPool{max: captchaPoolDefaultMax, ttl: captchaPoolDefaultTTL}

// configure applies host-config bounds (max pool size and token TTL). Invalid
// values fall back to defaults. Called on every PluginReconfigure.
func (p *captchaPool) configure(max int, ttl time.Duration) {
	p.Lock()
	defer p.Unlock()
	if max > 0 {
		p.max = max
	} else {
		p.max = captchaPoolDefaultMax
	}
	if ttl > 0 {
		p.ttl = ttl
	} else {
		p.ttl = captchaPoolDefaultTTL
	}
	p.dropExpired(time.Now())
}

func (p *captchaPool) add(param, region string, now time.Time) error {
	param = strings.TrimSpace(param)
	region = strings.TrimSpace(region)
	if len(param) < captchaPoolParamMin || len(param) > captchaPoolParamMax {
		return errors.New("invalid captcha parameter")
	}
	if len(region) > captchaPoolRegionMax {
		return errors.New("invalid captcha region")
	}
	p.Lock()
	defer p.Unlock()
	p.dropExpired(now)
	if len(p.items) >= p.max {
		return errors.New("captcha pool full")
	}
	p.items = append(p.items, captchaPoolToken{Param: param, Region: region, AddedAt: now})
	return nil
}

// take returns and permanently removes the oldest valid token (single-use).
func (p *captchaPool) take(now time.Time) (param, region string, ok bool) {
	p.Lock()
	defer p.Unlock()
	p.dropExpired(now)
	if len(p.items) == 0 {
		return "", "", false
	}
	head := p.items[0]
	copy(p.items, p.items[1:])
	p.items = p.items[:len(p.items)-1]
	return head.Param, head.Region, true
}

func (p *captchaPool) stats(now time.Time) (ready int) {
	p.Lock()
	defer p.Unlock()
	p.dropExpired(now)
	return len(p.items)
}

func (p *captchaPool) clear() {
	p.Lock()
	defer p.Unlock()
	p.items = nil
}

func (p *captchaPool) dropExpired(now time.Time) {
	kept := p.items[:0]
	for _, item := range p.items {
		if now.Sub(item.AddedAt) <= p.ttl {
			kept = append(kept, item)
		}
	}
	p.items = kept
}
