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
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/szxypi/cpa-devin-live-models/internal/core"
)

const abiVersion uint32 = 1

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

var (
	plugin      *core.Plugin
	pluginMu    sync.RWMutex
	closed      atomic.Bool
	hostCallsWg sync.WaitGroup
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, pluginAPI *C.cliproxy_plugin_api) C.int {
	if pluginAPI == nil {
		return 1
	}
	C.store_host_api(host)
	pluginAPI.abi_version = C.uint32_t(abiVersion)
	pluginAPI.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	pluginAPI.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	pluginAPI.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	closed.Store(false)
	pluginMu.Lock()
	plugin = core.NewPlugin(ffiHost{})
	pluginMu.Unlock()
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	defer func() {
		if r := recover(); r != nil {
			writeResponse(response, errorEnvelope("plugin_panic", "panic in plugin handler"))
			rc = 1
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var reqCopy []byte
	if request != nil && requestLen > 0 {
		reqCopy = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), reqCopy)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	closed.Store(true)
	pluginMu.RLock()
	p := plugin
	pluginMu.RUnlock()
	if p != nil {
		p.HandleShutdown()
	}
	done := make(chan struct{})
	go func() {
		hostCallsWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	pluginMu.RLock()
	p := plugin
	pluginMu.RUnlock()

	var out []byte
	var err error
	switch method {
	case "plugin.register", "plugin.reconfigure":
		out, err = p.HandleRegister(json.RawMessage(raw))
	case "plugin.quiesce":
		if p != nil {
			p.HandleQuiesce()
		}
		return okEnvelopeJSON(`{}`)
	case "plugin.shutdown":
		return okEnvelopeJSON(`{}`)
	case "model.static", "model.register":
		out, err = p.HandleModelStatic(json.RawMessage(raw))
	case "model.for_auth":
		out, err = p.HandleModelForAuth(json.RawMessage(raw))
	case "model.route":
		out, err = p.HandleModelRoute(json.RawMessage(raw))
	case "management.register":
		out, err = p.HandleManagementRegister(json.RawMessage(raw))
	case "management.handle":
		out, err = p.HandleManagementHandle(json.RawMessage(raw))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
	if err != nil {
		return nil, err
	}
	// every handler result must travel inside the RPC envelope
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(out)})
}

func okEnvelopeJSON(result string) ([]byte, error) {
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
}

func errorEnvelope(code, message string, httpStatus ...int) []byte {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	raw, _ := json.Marshal(envelope{
		OK: false,
		Error: &envelopeError{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
		},
	})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	buf := C.malloc(C.size_t(len(raw)))
	if buf == nil {
		return
	}
	copy((*[1 << 30]byte)(buf)[:len(raw)], raw)
	response.ptr = buf
	response.len = C.size_t(len(raw))
}

// ---------------- host call surface (plugin -> host) ----------------

// ffiHost implements core.Host over the C ABI. Host calls are allowed from
// any goroutine (the host resolves our identity via host_ctx), but never after
// shutdown — closed + WaitGroup guard against use-after-free.
type ffiHost struct{}

// Call invokes a host method and returns the decoded envelope.result.
func (ffiHost) Call(method string, payload []byte) (json.RawMessage, error) {
	if closed.Load() {
		return nil, errors.New("plugin is shutting down")
	}
	hostCallsWg.Add(1)
	defer hostCallsWg.Done()
	if closed.Load() {
		return nil, errors.New("plugin is shutting down")
	}

	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var reqPtr *C.uint8_t
	if len(payload) > 0 {
		buf := C.malloc(C.size_t(len(payload)))
		if buf == nil {
			return nil, errors.New("out of memory")
		}
		copy((*[1 << 30]byte)(buf)[:len(payload)], payload)
		reqPtr = (*C.uint8_t)(buf)
		defer C.free(buf)
	}

	var resp C.cliproxy_buffer
	resp.ptr = nil
	resp.len = 0
	rc := C.call_host_api(cMethod, reqPtr, C.size_t(len(payload)), &resp)
	if resp.ptr != nil {
		defer C.free_host_buffer(resp.ptr, resp.len)
	}
	if rc != 0 {
		return nil, errors.New("host call failed: " + method)
	}
	if resp.ptr == nil || resp.len == 0 {
		return json.RawMessage(`{}`), nil
	}
	raw := C.GoBytes(unsafe.Pointer(resp.ptr), C.int(resp.len))

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return json.RawMessage(raw), nil
	}
	if env.Error != nil {
		return nil, errors.New("host " + method + ": " + env.Error.Message)
	}
	if len(env.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return env.Result, nil
}

// Log emits a message through host.log. Best-effort, never fails upward.
func (h ffiHost) Log(level, message string, fields map[string]any) {
	req, err := json.Marshal(map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	})
	if err != nil {
		return
	}
	_, _ = h.Call("host.log", req)
}

var _ core.Host = ffiHost{}
