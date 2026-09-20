#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

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

static char last_host_log[4096];
static int host_log_count;
static int host_buffer_free_count;

static int mock_host_call(void *ctx, const char *method, const uint8_t *request, size_t request_len, cliproxy_buffer *response) {
    (void)ctx;
    if (method == NULL || strcmp(method, "host.log") != 0 || request == NULL || request_len >= sizeof(last_host_log)) {
        return 1;
    }
    memcpy(last_host_log, request, request_len);
    last_host_log[request_len] = '\0';
    host_log_count++;
    if (response != NULL) {
        response->ptr = malloc(1);
        if (response->ptr == NULL) {
            response->len = 0;
            return 1;
        }
        response->len = 1;
    }
    return 0;
}

static void mock_host_free(void *ptr, size_t len) {
    (void)len;
    host_buffer_free_count++;
    free(ptr);
}

static void reset_host_log(void) {
    host_log_count = 0;
    host_buffer_free_count = 0;
    last_host_log[0] = '\0';
}

static int call_plugin(cliproxy_plugin_api *api, const char *method, const char *request, char **response) {
    cliproxy_buffer raw = {0};
    int status = api->call((char *)method, (uint8_t *)request, strlen(request), &raw);
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

static int invoke(cliproxy_plugin_api *api, const char *stage, const char *method, const char *request, const char *must_contain, const char *must_not_contain) {
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

static int expect_log(const char *stage, const char *must_contain, const char *must_not_contain) {
    if (host_log_count == 1 && host_buffer_free_count == 1 && expect_contains(stage, last_host_log, must_contain) && expect_not_contains(stage, last_host_log, must_not_contain)) {
        return 1;
    }
    fprintf(stderr, "ABI host log failed at %s\n", stage);
    return 0;
}

static void make_state(char state[293], char value) {
    static const char alphabet[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    uint8_t raw[217] = {0};
    uint64_t issued = (uint64_t)time(NULL) - 60;
    raw[0] = 0x80;
    for (int i = 0; i < 8; i++) {
        raw[1 + i] = (uint8_t)(issued >> (8 * (7 - i)));
    }
    for (size_t i = 9; i < sizeof(raw); i++) {
        raw[i] = (uint8_t)i ^ (uint8_t)value;
    }
    size_t in = 0;
    size_t out = 0;
    while (in + 3 <= sizeof(raw)) {
        uint32_t block = ((uint32_t)raw[in] << 16) | ((uint32_t)raw[in + 1] << 8) | raw[in + 2];
        state[out++] = alphabet[(block >> 18) & 63];
        state[out++] = alphabet[(block >> 12) & 63];
        state[out++] = alphabet[(block >> 6) & 63];
        state[out++] = alphabet[block & 63];
        in += 3;
    }
    uint32_t tail = (uint32_t)raw[in] << 16;
    state[out++] = alphabet[(tail >> 18) & 63];
    state[out++] = alphabet[(tail >> 12) & 63];
    state[out++] = '=';
    state[out++] = '=';
    state[out] = '\0';
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

    cliproxy_host_api host = {.abi_version = 1, .call = mock_host_call, .free_buffer = mock_host_free};
    cliproxy_plugin_api api = {0};
    if (init(&host, &api) != 0 || api.abi_version != 1 || api.call == NULL || api.free_buffer == NULL || api.shutdown == NULL) {
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

    int ok = invoke(&api, "register", "plugin.register", "{\"schema_version\":6,\"config_yaml\":\"\"}", "\"ok\":true", NULL);
    ok = ok && invoke(&api, "bind", "request.intercept_after", "{\"RequestID\":\"capture\",\"ToFormat\":\"codex\",\"Model\":\"model-a\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", state_a);

    reset_host_log();
    snprintf(request, sizeof(request), "{\"RequestID\":\"capture\",\"ResponseHeaders\":{\"X-Codex-Turn-State\":[\"%s\"]},\"host_callback_id\":\"callback-capture\"}", state_a);
    ok = ok && invoke(&api, "capture", "response.intercept_after", request, "\"ok\":true", NULL);
    ok = ok && expect_log("capture log", "callback-capture", state_a);
    ok = ok && expect_not_contains("capture auth", last_host_log, "auth-a");

    reset_host_log();
    ok = ok && invoke(&api, "cross ip", "request.intercept_after", "{\"RequestID\":\"cross-ip\",\"ToFormat\":\"codex\",\"Model\":\"model-a\",\"Headers\":{\"X-Forwarded-For\":[\"203.0.113.10\"]},\"Metadata\":{\"selected_auth_id\":\"auth-a\"},\"host_callback_id\":\"callback-injected\"}", state_a, NULL);
    ok = ok && expect_log("injection log", "codex turn-state cache injected", state_a);
    ok = ok && invoke(&api, "different auth", "request.intercept_after", "{\"RequestID\":\"other-auth\",\"ToFormat\":\"codex\",\"Model\":\"model-a\",\"Metadata\":{\"selected_auth_id\":\"auth-b\"}}", "\"ok\":true", state_a);
    ok = ok && invoke(&api, "different model", "request.intercept_after", "{\"RequestID\":\"other-model\",\"ToFormat\":\"codex\",\"Model\":\"model-b\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", state_a);

    reset_host_log();
    snprintf(request, sizeof(request), "{\"RequestID\":\"capture\",\"ResponseHeaders\":{\"X-Codex-Turn-State\":[\"%s\"]}}", state_b);
    ok = ok && invoke(&api, "replace", "response.intercept_after", request, "\"ok\":true", NULL);
    ok = ok && expect_log("replace log", "replaced=true", state_b);
    ok = ok && invoke(&api, "replacement hit", "request.intercept_after", "{\"RequestID\":\"replacement\",\"ToFormat\":\"codex\",\"Model\":\"model-a\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", state_b, state_a);

    ok = ok && invoke(&api, "bind stream", "request.intercept_after", "{\"RequestID\":\"stream\",\"ToFormat\":\"codex\",\"Model\":\"model-stream\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", NULL);
    reset_host_log();
    snprintf(request, sizeof(request), "{\"RequestID\":\"stream\",\"ChunkIndex\":-1,\"ResponseHeaders\":{\"X-Codex-Turn-State\":[\"%s\"]}}", state_stream);
    ok = ok && invoke(&api, "stream header", "response.intercept_stream_chunk", request, "\"ok\":true", NULL);
    ok = ok && expect_log("stream log", "source=stream", state_stream);
    ok = ok && invoke(&api, "stream hit", "request.intercept_after", "{\"RequestID\":\"stream-hit\",\"ToFormat\":\"codex\",\"Model\":\"model-stream\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", state_stream, NULL);

    ok = ok && invoke(&api, "bind late", "request.intercept_after", "{\"RequestID\":\"late\",\"ToFormat\":\"codex\",\"Model\":\"model-late\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", NULL);
    ok = ok && invoke(&api, "complete", "request.complete", "{\"RequestID\":\"late\"}", "\"ok\":true", NULL);
    snprintf(request, sizeof(request), "{\"RequestID\":\"late\",\"ResponseHeaders\":{\"X-Codex-Turn-State\":[\"%s\"]}}", state_a);
    ok = ok && invoke(&api, "late response", "response.intercept_after", request, "\"ok\":true", NULL);
    ok = ok && invoke(&api, "late miss", "request.intercept_after", "{\"RequestID\":\"late-hit\",\"ToFormat\":\"codex\",\"Model\":\"model-late\",\"Metadata\":{\"selected_auth_id\":\"auth-a\"}}", "\"ok\":true", state_a);

    api.shutdown();
    dlclose(handle);
    if (!ok) {
        return 1;
    }
    printf("ABI_SMOKE_OK\n");
    return 0;
}
