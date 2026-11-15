// Command cpa-analytics is a CLIProxyAPI C ABI plugin. Build it with
// -buildmode=c-shared. All logic lives in internal/plugin; this file only
// moves bytes across the ABI.
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
	"fmt"
	"sync"
	"unsafe"

	"github.com/marcusgrando/cpa-analytics/internal/plugin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

var (
	instanceMu sync.Mutex
	instance   *plugin.Plugin
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if api == nil {
		return 1
	}
	C.store_host_api(host)
	api.abi_version = C.uint32_t(pluginabi.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	instanceMu.Lock()
	instance = plugin.New(cHost{})
	instanceMu.Unlock()
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	var out []byte
	status := C.int(0)
	func() {
		defer func() {
			if r := recover(); r != nil {
				out = []byte(fmt.Sprintf(`{"ok":false,"error":{"code":"plugin_panic","message":%q}}`, fmt.Sprint(r)))
				status = 1
			}
		}()
		if method == nil {
			out = []byte(`{"ok":false,"error":{"code":"invalid_method","message":"method is required"}}`)
			status = 1
			return
		}
		var req []byte
		if request != nil && requestLen > 0 {
			req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
		}
		instanceMu.Lock()
		p := instance
		instanceMu.Unlock()
		if p == nil {
			out = []byte(`{"ok":false,"error":{"code":"not_initialized","message":"plugin not initialized"}}`)
			status = 1
			return
		}
		out = p.Handle(C.GoString(method), req)
	}()
	writeResponse(response, out)
	return status
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() { _ = recover() }()
	instanceMu.Lock()
	p := instance
	instanceMu.Unlock()
	if p != nil {
		p.Close()
	}
}

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

// cHost calls host callbacks such as host.http.do and host.log.
type cHost struct{}

func (cHost) Call(method string, req []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var reqPtr *C.uint8_t
	if len(req) > 0 {
		reqPtr = (*C.uint8_t)(C.CBytes(req))
		defer C.free(unsafe.Pointer(reqPtr))
	}
	var resp C.cliproxy_buffer
	rc := C.call_host_api(cMethod, reqPtr, C.size_t(len(req)), &resp)
	var raw []byte
	if resp.ptr != nil && resp.len > 0 {
		raw = C.GoBytes(resp.ptr, C.int(resp.len))
		C.free_host_buffer(resp.ptr, resp.len)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("decode %s response: %w", method, err)
		}
	}
	if rc != 0 || !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, errors.New(method + ": host call failed")
	}
	return env.Result, nil
}
