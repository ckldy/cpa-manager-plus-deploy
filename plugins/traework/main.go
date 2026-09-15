// TRAE Work CPA plugin.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// cliproxy_plugin_init is the C ABI entry point.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { stopRuntime() }

func main() {}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

var callHostRPC = callHost

var runtime struct {
	sync.RWMutex
	cancel    context.CancelFunc
	done      chan struct{}
	lastRun   time.Time
	nextRun   time.Time
	lastError string
}

type runtimeStatus struct {
	Running                     bool
	LastRun, NextRun, LastError string
}

func runtimeSnapshot() runtimeStatus {
	runtime.RLock()
	defer runtime.RUnlock()
	format := func(t time.Time) string {
		if t.IsZero() {
			return "尚未运行"
		}
		return t.In(hongKongLocation()).Format("2006-01-02 15:04:05 MST")
	}
	return runtimeStatus{Running: runtime.cancel != nil, LastRun: format(runtime.lastRun), NextRun: format(runtime.nextRun), LastError: runtime.lastError}
}
func hongKongLocation() *time.Location {
	loc, e := time.LoadLocation("Asia/Hong_Kong")
	if e != nil {
		return time.FixedZone("Asia/Hong_Kong", 8*3600)
	}
	return loc
}
func nextDailyRun(now time.Time) time.Time {
	loc := hongKongLocation()
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), 9, 0, 0, 0, loc)
	if !next.After(n) {
		next = next.Add(24 * time.Hour)
	}
	return next
}
func stableAccountJitter(key string) time.Duration {
	sum := sha256.Sum256([]byte("traework-checkin:" + key))
	return time.Duration(binary.BigEndian.Uint32(sum[:4])%1800) * time.Second
}
func discoverScheduledAccounts(ctx context.Context, auth HostAuth) ([]ScheduledAccount, error) {
	files, e := auth.List(ctx)
	if e != nil {
		return nil, e
	}
	out := make([]ScheduledAccount, 0, len(files))
	for _, f := range files {
		if validTraeWorkFile(f) {
			out = append(out, ScheduledAccount{AuthIndex: f.AuthIndex, Name: f.Name, Disabled: f.Disabled})
		}
	}
	return out, nil
}
func runScheduledDay(ctx context.Context, now time.Time) error {
	auth := HostAuth{Call: callHostRPC}
	accounts, e := discoverScheduledAccounts(ctx, auth)
	if e != nil {
		return e
	}
	client := traeAccountClient{auth: auth}
	scheduler := NewDailyScheduler(DailySchedulerConfig{Client: client, Concurrency: 2})
	var wg sync.WaitGroup
	sem := make(chan struct{}, 2)
	errCh := make(chan error, len(accounts))
	for _, account := range accounts {
		account := account
		if account.Disabled {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			timer := time.NewTimer(stableAccountJitter(accountKey(account)))
			select {
			case <-ctx.Done():
				timer.Stop()
				errCh <- ctx.Err()
				return
			case <-timer.C:
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
			defer func() { <-sem }()
			if e := scheduler.RunDay(ctx, now, []ScheduledAccount{account}); e != nil {
				errCh <- e
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			return e
		}
	}
	return nil
}
func startRuntime() {
	runtime.Lock()
	if runtime.cancel != nil {
		runtime.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime.cancel = cancel
	runtime.done = make(chan struct{})
	runtime.nextRun = nextDailyRun(time.Now())
	done := runtime.done
	runtime.Unlock()
	// Populate and persist the model cache shortly after startup so the host
	// exposes the real TRAE model list even before any manual refresh.
	go refreshModelsOnce()
	go func() {
		defer close(done)
		for {
			runtime.RLock()
			next := runtime.nextRun
			runtime.RUnlock()
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case now := <-timer.C:
				e := runScheduledDay(ctx, now)
				runtime.Lock()
				runtime.lastRun = now
				if e != nil {
					runtime.lastError = e.Error()
				} else {
					runtime.lastError = "成功"
				}
				runtime.nextRun = nextDailyRun(now.Add(time.Second))
				runtime.Unlock()
			}
		}
	}()
}
func stopRuntime() {
	runtime.Lock()
	cancel, done := runtime.cancel, runtime.done
	runtime.cancel = nil
	runtime.done = nil
	runtime.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// refreshModelsOnce fetches the live model list after startup when a valid
// traework account exists. It populates the in-memory cache and persists it
// into the auth file, which wakes host re-registration so /v1/models exposes
// the real models shortly after boot instead of only the static fallback.
// It is best-effort and never blocks plugin registration.
func refreshModelsOnce() {
	auth := HostAuth{Call: callHostRPC}
	accounts, err := discoverScheduledAccounts(context.Background(), auth)
	if err != nil {
		return
	}
	for _, account := range accounts {
		if account.Disabled {
			continue
		}
		doc, err := auth.Get(context.Background(), account.AuthIndex)
		if err != nil {
			continue
		}
		s, err := parseAuthStorage(doc.JSON)
		if err != nil || strings.TrimSpace(s.AccessToken) == "" {
			continue
		}
		if _, err := refreshTraeModels("", s, account.Name); err == nil {
			return
		}
	}
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}

	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}
