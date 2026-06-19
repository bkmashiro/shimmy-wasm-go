// A tiny C++ evaluation function that can be compiled directly to WebAssembly.
//
// It intentionally avoids libc/libc++ so the build is just a single freestanding
// C++ source file plus Zig's wasm-capable clang driver. The evaluator follows
// Shimmy's internal WASM ABI:
//   - export memory
//   - alloc(size) -> request pointer
//   - evaluate(ptr, len) -> pointer to [u32 little-endian length][JSON bytes]
//
// The business logic is deliberately Lambda Feedback-shaped: compare the
// submitted response with the answer and return feedback from params.

using u32 = unsigned int;
using i32 = int;
using usize = __SIZE_TYPE__;
using uintptr = __UINTPTR_TYPE__;

alignas(16) static char request_buffer[256 * 1024];
alignas(16) static char response_buffer[256 * 1024];

// Mutable guest state. Shimmy should restore the warm instance snapshot after
// every request, so this must be 1 for every HTTP call when FUNCTION_MAX_PROCS=1.
static u32 invocation_count = 0;

static usize cstr_len(const char *s) {
  usize n = 0;
  while (s[n] != 0) n++;
  return n;
}

static bool bytes_equal(const char *a, usize a_len, const char *b, usize b_len) {
  if (a_len != b_len) return false;
  for (usize i = 0; i < a_len; i++) {
    if (a[i] != b[i]) return false;
  }
  return true;
}

static void copy_bytes(char *dst, usize &pos, const char *src, usize len) {
  for (usize i = 0; i < len; i++) dst[pos++] = src[i];
}

static void append_cstr(char *dst, usize &pos, const char *src) {
  copy_bytes(dst, pos, src, cstr_len(src));
}

static void append_u32(char *dst, usize &pos, u32 value) {
  char tmp[10];
  usize n = 0;
  do {
    tmp[n++] = char('0' + (value % 10));
    value /= 10;
  } while (value != 0);
  while (n > 0) dst[pos++] = tmp[--n];
}

static void append_json_string(char *dst, usize &pos, const char *src, usize len) {
  dst[pos++] = '"';
  for (usize i = 0; i < len; i++) {
    char c = src[i];
    if (c == '"' || c == '\\') {
      dst[pos++] = '\\';
      dst[pos++] = c;
    } else if (c == '\n') {
      dst[pos++] = '\\';
      dst[pos++] = 'n';
    } else {
      dst[pos++] = c;
    }
  }
  dst[pos++] = '"';
}

static bool match_at(const char *json, usize len, usize pos, const char *needle) {
  for (usize i = 0; needle[i] != 0; i++) {
    if (pos + i >= len || json[pos + i] != needle[i]) return false;
  }
  return true;
}

static bool find_json_string(const char *json, usize len, const char *key,
                             const char *&value, usize &value_len) {
  char quoted_key[96];
  usize key_pos = 0;
  quoted_key[key_pos++] = '"';
  for (usize i = 0; key[i] != 0 && key_pos + 2 < sizeof(quoted_key); i++) {
    quoted_key[key_pos++] = key[i];
  }
  quoted_key[key_pos++] = '"';
  quoted_key[key_pos] = 0;

  for (usize i = 0; i < len; i++) {
    if (!match_at(json, len, i, quoted_key)) continue;
    i += key_pos;
    while (i < len && (json[i] == ' ' || json[i] == '\n' || json[i] == '\r' || json[i] == '\t')) i++;
    if (i >= len || json[i] != ':') continue;
    i++;
    while (i < len && (json[i] == ' ' || json[i] == '\n' || json[i] == '\r' || json[i] == '\t')) i++;
    if (i >= len || json[i] != '"') continue;
    i++;

    usize start = i;
    while (i < len) {
      if (json[i] == '\\') {
        i += 2;
        continue;
      }
      if (json[i] == '"') {
        value = json + start;
        value_len = i - start;
        return true;
      }
      i++;
    }
  }
  return false;
}

static i32 write_error(const char *message) {
  usize pos = 4;
  append_cstr(response_buffer, pos, "{\"error\":{\"message\":");
  append_json_string(response_buffer, pos, message, cstr_len(message));
  append_cstr(response_buffer, pos, "}}"
  );
  u32 len = u32(pos - 4);
  response_buffer[0] = char(len & 0xff);
  response_buffer[1] = char((len >> 8) & 0xff);
  response_buffer[2] = char((len >> 16) & 0xff);
  response_buffer[3] = char((len >> 24) & 0xff);
  return i32(uintptr(response_buffer));
}

static i32 write_eval_response(bool is_correct,
                               const char *feedback,
                               usize feedback_len) {
  usize pos = 4;
  append_cstr(response_buffer, pos, "{\"command\":\"eval\",\"result\":{");
  append_cstr(response_buffer, pos, "\"is_correct\":");
  append_cstr(response_buffer, pos, is_correct ? "true" : "false");
  append_cstr(response_buffer, pos, ",\"feedback\":");
  append_json_string(response_buffer, pos, feedback, feedback_len);
  append_cstr(response_buffer, pos, ",\"guest_invocation_count\":");
  append_u32(response_buffer, pos, invocation_count);
  append_cstr(response_buffer, pos, ",\"snapshot_isolation_ok\":");
  append_cstr(response_buffer, pos, invocation_count == 1 ? "true" : "false");
  append_cstr(response_buffer, pos, "}}"
  );

  u32 len = u32(pos - 4);
  response_buffer[0] = char(len & 0xff);
  response_buffer[1] = char((len >> 8) & 0xff);
  response_buffer[2] = char((len >> 16) & 0xff);
  response_buffer[3] = char((len >> 24) & 0xff);
  return i32(uintptr(response_buffer));
}

extern "C" i32 alloc(i32 size) {
  if (size <= 0 || usize(size) > sizeof(request_buffer)) return 0;
  return i32(uintptr(request_buffer));
}

extern "C" i32 evaluate(i32 req_ptr, i32 req_len) {
  if (req_ptr == 0 || req_len <= 0) return write_error("empty request");

  const char *json = reinterpret_cast<const char *>(uintptr(req_ptr));
  usize len = usize(req_len);

  const char *method = nullptr;
  const char *response = nullptr;
  const char *answer = nullptr;
  const char *correct_feedback = nullptr;
  const char *incorrect_feedback = nullptr;
  usize method_len = 0;
  usize response_len = 0;
  usize answer_len = 0;
  usize correct_feedback_len = 0;
  usize incorrect_feedback_len = 0;

  if (!find_json_string(json, len, "method", method, method_len)) return write_error("missing method");
  if (!bytes_equal(method, method_len, "eval", 4)) return write_error("unsupported method");
  if (!find_json_string(json, len, "response", response, response_len)) return write_error("missing response");
  if (!find_json_string(json, len, "answer", answer, answer_len)) return write_error("missing answer");

  bool has_correct_feedback = find_json_string(json, len, "correct_response_feedback", correct_feedback, correct_feedback_len);
  bool has_incorrect_feedback = find_json_string(json, len, "incorrect_response_feedback", incorrect_feedback, incorrect_feedback_len);

  invocation_count++;

  bool is_correct = bytes_equal(response, response_len, answer, answer_len);
  if (is_correct) {
    if (!has_correct_feedback) {
      correct_feedback = "Correct";
      correct_feedback_len = 7;
    }
    return write_eval_response(true, correct_feedback, correct_feedback_len);
  }

  if (!has_incorrect_feedback) {
    incorrect_feedback = "Incorrect";
    incorrect_feedback_len = 9;
  }
  return write_eval_response(false, incorrect_feedback, incorrect_feedback_len);
}
