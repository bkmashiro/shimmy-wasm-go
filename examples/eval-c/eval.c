/*
 * Shimmy eval function — C / wasm32-wasip1
 *
 * Build (requires WASI-SDK, e.g. /opt/wasi-sdk):
 *   make
 *
 * Run:
 *   FUNCTION_INTERFACE=wasm FUNCTION_COMMAND=./eval.wasm ./shimmy serve
 *
 * JSON parsing is done with jsmn (single-header, no malloc).
 * JSON responses are built with snprintf — sufficient for the simple eval ABI.
 */

#include <stdint.h>
#include <string.h>
#include <stdio.h>

#define JSMN_STATIC
#include "jsmn.h"

/* Static buffers — WASM is single-threaded; one request at a time. */
static uint8_t req_buf[256 * 1024];
static uint8_t resp_buf[256 * 1024];

/* Helper: write a length-prefixed JSON string into resp_buf. */
static void write_resp(const char *json) {
    uint32_t len = (uint32_t)strlen(json);
    /* 4-byte little-endian length prefix */
    resp_buf[0] = (uint8_t)(len & 0xff);
    resp_buf[1] = (uint8_t)((len >> 8) & 0xff);
    resp_buf[2] = (uint8_t)((len >> 16) & 0xff);
    resp_buf[3] = (uint8_t)((len >> 24) & 0xff);
    memcpy(resp_buf + 4, json, len);
}

/* Helper: compare a jsmn token to a string literal. */
static int tok_eq(const char *src, jsmntok_t *tok, const char *s) {
    int len = tok->end - tok->start;
    return tok->type == JSMN_STRING &&
           (int)strlen(s) == len &&
           strncmp(src + tok->start, s, len) == 0;
}

/* Helper: copy a jsmn token's value into dst (NUL-terminated). */
static void tok_str(const char *src, jsmntok_t *tok, char *dst, size_t cap) {
    int len = tok->end - tok->start;
    if (len >= (int)cap) len = (int)cap - 1;
    memcpy(dst, src + tok->start, len);
    dst[len] = '\0';
}

/*
 * alloc — called by host before each request.
 * Returns pointer to req_buf (host writes the JSON payload here).
 */
__attribute__((visibility("default")))
int32_t alloc(int32_t size) {
    (void)size;
    return (int32_t)(uintptr_t)req_buf;
}

/*
 * dispatch — called by host with (ptr, len) of the JSON request in linear memory.
 * Returns pointer to resp_buf: [4-byte LE length][JSON body].
 */
__attribute__((visibility("default")))
int32_t dispatch(int32_t req_ptr, int32_t req_len) {
    (void)req_ptr; /* always &req_buf */

    const char *src = (const char *)req_buf;
    int len = req_len;

    /* Parse JSON with jsmn */
    jsmntok_t toks[128];
    jsmn_parser parser;
    jsmn_init(&parser);
    int n = jsmn_parse(&parser, src, len, toks, 128);

    if (n < 1 || toks[0].type != JSMN_OBJECT) {
        write_resp("{\"error\":{\"message\":\"invalid JSON\"}}");
        return (int32_t)(uintptr_t)resp_buf;
    }

    /* Find "method" and "params" keys */
    char method[64] = {0};
    int params_idx = -1;

    for (int i = 1; i < n - 1; i++) {
        if (tok_eq(src, &toks[i], "method") && toks[i+1].type == JSMN_STRING) {
            tok_str(src, &toks[i+1], method, sizeof(method));
        }
        if (tok_eq(src, &toks[i], "params") && toks[i+1].type == JSMN_OBJECT) {
            params_idx = i + 1;
        }
    }

    if (strcmp(method, "healthcheck") == 0) {
        write_resp("{\"command\":\"healthcheck\",\"result\":{\"status\":\"ok\"}}");

    } else if (strcmp(method, "eval") == 0) {
        char response[4096] = {0};
        char answer[4096]   = {0};

        if (params_idx >= 0) {
            int pend = params_idx + toks[params_idx].size * 2 + 1;
            for (int i = params_idx + 1; i < n - 1 && i < pend; i++) {
                if (tok_eq(src, &toks[i], "response") && toks[i+1].type == JSMN_STRING)
                    tok_str(src, &toks[i+1], response, sizeof(response));
                if (tok_eq(src, &toks[i], "answer") && toks[i+1].type == JSMN_STRING)
                    tok_str(src, &toks[i+1], answer, sizeof(answer));
            }
        }

        int is_correct = (strcmp(response, answer) == 0);
        char out[512];
        snprintf(out, sizeof(out),
            "{\"command\":\"eval\",\"result\":{\"is_correct\":%s,\"feedback\":\"%s\"}}",
            is_correct ? "true" : "false",
            is_correct ? "Correct!" : "Incorrect.");
        write_resp(out);

    } else if (strcmp(method, "preview") == 0) {
        char response[4096] = {0};

        if (params_idx >= 0) {
            int pend = params_idx + toks[params_idx].size * 2 + 1;
            for (int i = params_idx + 1; i < n - 1 && i < pend; i++) {
                if (tok_eq(src, &toks[i], "response") && toks[i+1].type == JSMN_STRING)
                    tok_str(src, &toks[i+1], response, sizeof(response));
            }
        }

        char out[8192];
        snprintf(out, sizeof(out),
            "{\"command\":\"preview\",\"result\":{\"preview\":{\"type\":\"text\",\"content\":\"%s\"}}}",
            response);
        write_resp(out);

    } else {
        char out[256];
        snprintf(out, sizeof(out),
            "{\"error\":{\"message\":\"unknown method: %s\"}}", method);
        write_resp(out);
    }

    return (int32_t)(uintptr_t)resp_buf;
}
