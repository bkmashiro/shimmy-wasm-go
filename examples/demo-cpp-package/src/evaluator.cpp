#include "compare.hpp"

using u32 = unsigned int;
using i32 = int;
using uintptr = __UINTPTR_TYPE__;

alignas(16) static char request_buffer[256 * 1024];
alignas(16) static char response_buffer[256 * 1024];
static u32 invocation_count = 0;

static usize cstr_len(const char *s) {
  usize n = 0;
  while (s[n] != 0) n++;
  return n;
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

static void append_json_string(char *dst, usize &pos, TextView src) {
  dst[pos++] = '"';
  for (usize i = 0; i < src.len; i++) {
    char c = src.ptr[i];
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

static bool find_json_string(const char *json, usize len, const char *key, TextView &value) {
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
        value = TextView{json + start, i - start};
        return true;
      }
      i++;
    }
  }
  return false;
}

static i32 write_eval_response(bool is_correct, TextView feedback) {
  usize pos = 4;
  append_cstr(response_buffer, pos, "{\"command\":\"eval\",\"result\":{");
  append_cstr(response_buffer, pos, "\"is_correct\":");
  append_cstr(response_buffer, pos, is_correct ? "true" : "false");
  append_cstr(response_buffer, pos, ",\"feedback\":");
  append_json_string(response_buffer, pos, feedback);
  append_cstr(response_buffer, pos, ",\"guest_invocation_count\":");
  append_u32(response_buffer, pos, invocation_count);
  append_cstr(response_buffer, pos, ",\"snapshot_isolation_ok\":");
  append_cstr(response_buffer, pos, invocation_count == 1 ? "true" : "false");
  append_cstr(response_buffer, pos, "}}");

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
  if (req_ptr == 0 || req_len <= 0) return 0;
  const char *json = reinterpret_cast<const char *>(uintptr(req_ptr));
  usize len = usize(req_len);

  TextView response{nullptr, 0};
  TextView answer{nullptr, 0};
  TextView correct_feedback{nullptr, 0};
  TextView incorrect_feedback{nullptr, 0};

  if (!find_json_string(json, len, "response", response)) return 0;
  if (!find_json_string(json, len, "answer", answer)) return 0;
  find_json_string(json, len, "correct_response_feedback", correct_feedback);
  find_json_string(json, len, "incorrect_response_feedback", incorrect_feedback);

  invocation_count++;
  bool is_correct = bytes_equal(response, answer);
  return write_eval_response(is_correct, feedback_for(is_correct, correct_feedback, incorrect_feedback));
}
