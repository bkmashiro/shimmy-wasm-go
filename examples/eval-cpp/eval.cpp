// Shimmy eval function — C++ / wasm32-wasip1
//
// Build (requires WASI-SDK, e.g. /opt/wasi-sdk):
//   make
//
// Run:
//   FUNCTION_INTERFACE=wasm FUNCTION_COMMAND=./eval.wasm ./shimmy serve

#include <cstdint>
#include <cstring>
#include <string>
#include <string_view>
#include <charconv>

// ── Minimal JSON helpers ──────────────────────────────────────────────────────
// We avoid pulling in a full JSON library to keep the binary small.
// The request format is always {"method":"...","params":{...}} which is simple
// enough to parse with string_view searches.

static std::string_view json_str(std::string_view src, std::string_view key) {
    // Find  "key":"value"  and return the value string (unescaped, no nesting).
    std::string needle = "\"";
    needle += key;
    needle += "\":\"";
    auto pos = src.find(needle);
    if (pos == std::string_view::npos) return {};
    pos += needle.size();
    auto end = src.find('"', pos);
    if (end == std::string_view::npos) return {};
    return src.substr(pos, end - pos);
}

static std::string_view json_obj(std::string_view src, std::string_view key) {
    // Find  "key":{...}  and return the object (shallow — assumes no nested {}).
    std::string needle = "\"";
    needle += key;
    needle += "\":{";
    auto pos = src.find(needle);
    if (pos == std::string_view::npos) return {};
    pos += needle.size() - 1; // include the opening {
    auto end = src.find('}', pos);
    if (end == std::string_view::npos) return {};
    return src.substr(pos, end - pos + 1);
}

// ── Static buffers ────────────────────────────────────────────────────────────
// WASM is single-threaded; one request at a time.

static uint8_t req_buf[256 * 1024];
static uint8_t resp_buf[256 * 1024];

static void write_resp(std::string_view json) {
    uint32_t len = static_cast<uint32_t>(json.size());
    resp_buf[0] = static_cast<uint8_t>(len & 0xff);
    resp_buf[1] = static_cast<uint8_t>((len >> 8) & 0xff);
    resp_buf[2] = static_cast<uint8_t>((len >> 16) & 0xff);
    resp_buf[3] = static_cast<uint8_t>((len >> 24) & 0xff);
    std::memcpy(resp_buf + 4, json.data(), json.size());
}

// ── Guest ABI ─────────────────────────────────────────────────────────────────

extern "C" {

/// Called by the host before each request to obtain the write pointer.
__attribute__((visibility("default")))
int32_t alloc(int32_t /*size*/) {
    return static_cast<int32_t>(reinterpret_cast<uintptr_t>(req_buf));
}

/// Called with (ptr, len) of the JSON request payload in linear memory.
/// Returns a pointer to [4-byte LE length][JSON response body] in resp_buf.
__attribute__((visibility("default")))
int32_t dispatch(int32_t /*req_ptr*/, int32_t req_len) {
    std::string_view src(reinterpret_cast<char*>(req_buf),
                         static_cast<size_t>(req_len));

    auto method = json_str(src, "method");
    auto params = json_obj(src, "params");

    if (method == "healthcheck") {
        write_resp(R"({"command":"healthcheck","result":{"status":"ok"}})");

    } else if (method == "eval") {
        auto response = json_str(params, "response");
        auto answer   = json_str(params, "answer");
        bool correct  = (response == answer);
        if (correct) {
            write_resp(R"({"command":"eval","result":{"is_correct":true,"feedback":"Correct!"}})");
        } else {
            write_resp(R"({"command":"eval","result":{"is_correct":false,"feedback":"Incorrect."}})");
        }

    } else if (method == "preview") {
        auto response = json_str(params, "response");
        // Build the response with the response value embedded.
        std::string out;
        out.reserve(128 + response.size());
        out += R"({"command":"preview","result":{"preview":{"type":"text","content":")";
        out += response;
        out += R"("}}})";
        write_resp(out);

    } else {
        std::string out;
        out += R"({"error":{"message":"unknown method: )";
        out += method;
        out += R"("}})";
        write_resp(out);
    }

    return static_cast<int32_t>(reinterpret_cast<uintptr_t>(resp_buf));
}

} // extern "C"
