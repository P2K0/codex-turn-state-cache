#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
    void *ptr;
    size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void *, const char *, const uint8_t *, size_t, cliproxy_buffer *);
typedef void (*cliproxy_host_free_fn)(void *, size_t);

typedef struct {
    uint32_t abi_version;
    void *host_ctx;
    cliproxy_host_call_fn call;
    cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char *, uint8_t *, size_t, cliproxy_buffer *);
typedef void (*cliproxy_plugin_free_fn)(void *, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
    uint32_t abi_version;
    cliproxy_plugin_call_fn call;
    cliproxy_plugin_free_fn free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

typedef int (*cliproxy_plugin_init_fn)(const cliproxy_host_api *, cliproxy_plugin_api *);

static int call_plugin(cliproxy_plugin_api *api, const char *method, const char *request, char **response) {
    cliproxy_buffer raw = {0};
    size_t length = strlen(request);
    int status = api->call((char *)method, (uint8_t *)request, length, &raw);
    if (status != 0 || raw.ptr == NULL || raw.len == 0) {
        if (raw.ptr != NULL) {
            api->free_buffer(raw.ptr, raw.len);
        }
        return 0;
    }

    *response = calloc(raw.len + 1, 1);
    if (*response == NULL) {
        api->free_buffer(raw.ptr, raw.len);
        return 0;
    }
    memcpy(*response, raw.ptr, raw.len);
    api->free_buffer(raw.ptr, raw.len);
    return 1;
}

static int expect_contains(const char *stage, const char *response, const char *value) {
    if (response != NULL && strstr(response, value) != NULL) {
        return 1;
    }
    fprintf(stderr, "ABI smoke failed at %s\n", stage);
    return 0;
}

static int expect_not_contains(const char *stage, const char *response, const char *value) {
    if (response == NULL || strstr(response, value) == NULL) {
        return 1;
    }
    fprintf(stderr, "ABI smoke failed at %s\n", stage);
    return 0;
}

static int invoke_expect(cliproxy_plugin_api *api, const char *stage, const char *method, const char *request, const char *must_contain, const char *must_not_contain) {
    char *response = NULL;
    int ok = call_plugin(api, method, request, &response);
    if (ok && must_contain != NULL) {
        ok = expect_contains(stage, response, must_contain);
    }
    if (ok && must_not_contain != NULL) {
        ok = expect_not_contains(stage, response, must_not_contain);
    }
    free(response);
    return ok;
}

static void make_state(char state[293], char value) {
    memset(state, value, 292);
    state[292] = '\0';
}

int main(int argc, char **argv) {
    if (argc != 2) {
        fprintf(stderr, "usage: %s <plugin.so>\n", argv[0]);
        return 2;
    }

    void *handle = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    if (handle == NULL) {
        fprintf(stderr, "unable to load plugin\n");
        return 1;
    }
    cliproxy_plugin_init_fn init = (cliproxy_plugin_init_fn)dlsym(handle, "cliproxy_plugin_init");
    if (init == NULL) {
        fprintf(stderr, "missing plugin entry point\n");
        dlclose(handle);
        return 1;
    }

    cliproxy_host_api host = { .abi_version = 1 };
    cliproxy_plugin_api api = {0};
    if (init(&host, &api) != 0 || api.abi_version != 1 || api.call == NULL || api.free_buffer == NULL) {
        fprintf(stderr, "invalid plugin ABI table\n");
        dlclose(handle);
        return 1;
    }

    char state_a[293];
    char state_b[293];
    char state_stream[293];
    char request[1024];
    make_state(state_a, 'a');
    make_state(state_b, 'b');
    make_state(state_stream, 's');

    int ok = 1;
    ok = ok && invoke_expect(&api, "register", "plugin.register", "{\"schema_version\":6,\"config_yaml\":\"\"}", "\"ok\":true", NULL);
    ok = ok && invoke_expect(&api, "bind first request", "request.intercept_after", "{\"RequestID\":\"capture\",\"ToFormat\":\"codex\",\"Model\":\"model-a\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", state_a);

    snprintf(request, sizeof(request), "{\"RequestID\":\"capture\",\"ResponseHeaders\":{\"X-Codex-Turn-State\":[\"%s\"]}}", state_a);
    ok = ok && invoke_expect(&api, "capture first state", "response.intercept_after", request, "\"ok\":true", NULL);

    ok = ok && invoke_expect(&api, "same key cross ip", "request.intercept_after", "{\"RequestID\":\"cross-ip\",\"ToFormat\":\"codex\",\"Model\":\"model-a\",\"Headers\":{\"X-Forwarded-For\":[\"203.0.113.10\"]},\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", state_a, NULL);
    ok = ok && invoke_expect(&api, "different auth miss", "request.intercept_after", "{\"RequestID\":\"other-auth\",\"ToFormat\":\"codex\",\"Model\":\"model-a\",\"Metadata\":{\"selected_auth_id\":\"auth-b\"}}", "\"ok\":true", state_a);
    ok = ok && invoke_expect(&api, "different model miss", "request.intercept_after", "{\"RequestID\":\"other-model\",\"ToFormat\":\"codex\",\"Model\":\"model-b\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", state_a);

    snprintf(request, sizeof(request), "{\"RequestID\":\"capture\",\"ResponseHeaders\":{\"X-Codex-Turn-State\":[\"%s\"]}}", state_b);
    ok = ok && invoke_expect(&api, "replace state", "response.intercept_after", request, "\"ok\":true", NULL);
    ok = ok && invoke_expect(&api, "replacement hit", "request.intercept_after", "{\"RequestID\":\"replacement\",\"ToFormat\":\"codex\",\"Model\":\"model-a\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", state_b, state_a);

    ok = ok && invoke_expect(&api, "bind stream", "request.intercept_after", "{\"RequestID\":\"stream\",\"ToFormat\":\"codex\",\"Model\":\"model-stream\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", NULL);
    snprintf(request, sizeof(request), "{\"RequestID\":\"stream\",\"ChunkIndex\":-1,\"ResponseHeaders\":{\"X-Codex-Turn-State\":[\"%s\"]}}", state_stream);
    ok = ok && invoke_expect(&api, "capture stream header", "response.intercept_stream_chunk", request, "\"ok\":true", NULL);
    ok = ok && invoke_expect(&api, "stream state hit", "request.intercept_after", "{\"RequestID\":\"stream-hit\",\"ToFormat\":\"codex\",\"Model\":\"model-stream\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", state_stream, NULL);

    ok = ok && invoke_expect(&api, "bind late response", "request.intercept_after", "{\"RequestID\":\"late\",\"ToFormat\":\"codex\",\"Model\":\"model-late\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", NULL);
    ok = ok && invoke_expect(&api, "complete request", "request.complete", "{\"RequestID\":\"late\"}", "\"ok\":true", NULL);
    snprintf(request, sizeof(request), "{\"RequestID\":\"late\",\"ResponseHeaders\":{\"X-Codex-Turn-State\":[\"%s\"]}}", state_a);
    ok = ok && invoke_expect(&api, "late response ignored", "response.intercept_after", request, "\"ok\":true", NULL);
    ok = ok && invoke_expect(&api, "late state miss", "request.intercept_after", "{\"RequestID\":\"late-hit\",\"ToFormat\":\"codex\",\"Model\":\"model-late\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", state_a);

    if (api.shutdown != NULL) {
        api.shutdown();
    }
    dlclose(handle);
    if (!ok) {
        return 1;
    }
    printf("ABI_SMOKE_OK\n");
    return 0;
}
