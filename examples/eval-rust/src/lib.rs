// Shimmy eval function — Rust / wasm32-wasip1
//
// Build:
//   rustup target add wasm32-wasip1
//   cargo build --target wasm32-wasip1 --release
//   cp target/wasm32-wasip1/release/eval_rust.wasm eval.wasm
//
// Run:
//   FUNCTION_INTERFACE=wasm FUNCTION_COMMAND=./eval.wasm ./shimmy serve

use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::collections::HashMap;

// Static buffers — WASM is single-threaded; one request at a time.
static mut REQ_BUF: [u8; 256 * 1024] = [0u8; 256 * 1024];
static mut RESP_BUF: [u8; 256 * 1024] = [0u8; 256 * 1024];

/// Called by the host before every request to get a pointer for the request payload.
#[no_mangle]
pub unsafe extern "C" fn alloc(_size: i32) -> i32 {
    REQ_BUF.as_ptr() as i32
}

/// Called by the host with the pointer and length of the JSON request in linear memory.
/// Returns a pointer to a 4-byte LE length prefix followed by the JSON response body.
#[no_mangle]
pub unsafe extern "C" fn dispatch(_req_ptr: i32, req_len: i32) -> i32 {
    let req_bytes = &REQ_BUF[..req_len as usize];

    let json = match serde_json::from_slice::<Value>(req_bytes) {
        Ok(v) => v,
        Err(e) => {
            write_error(&format!("JSON parse error: {e}"));
            return RESP_BUF.as_ptr() as i32;
        }
    };

    let method = json["method"].as_str().unwrap_or("");
    let params = &json["params"];

    let resp: Value = match method {
        "eval" => {
            let response = params["response"].as_str().unwrap_or("");
            let answer = params["answer"].as_str().unwrap_or("");
            let is_correct = response == answer;
            let feedback = if is_correct { "Correct!" } else { "Incorrect." };
            serde_json::json!({
                "command": "eval",
                "result": {
                    "is_correct": is_correct,
                    "feedback": feedback,
                }
            })
        }
        "preview" => {
            let response = params["response"].as_str().unwrap_or("");
            serde_json::json!({
                "command": "preview",
                "result": {
                    "preview": {
                        "type": "text",
                        "content": response,
                    }
                }
            })
        }
        "healthcheck" => {
            serde_json::json!({
                "command": "healthcheck",
                "result": { "status": "ok" }
            })
        }
        _ => {
            serde_json::json!({
                "error": { "message": format!("unknown method: {method}") }
            })
        }
    };

    write_resp(&resp);
    RESP_BUF.as_ptr() as i32
}

unsafe fn write_resp(v: &Value) {
    let data = serde_json::to_vec(v)
        .unwrap_or_else(|_| br#"{"error":{"message":"marshal failed"}}"#.to_vec());
    let len = data.len() as u32;
    RESP_BUF[0..4].copy_from_slice(&len.to_le_bytes());
    RESP_BUF[4..4 + data.len()].copy_from_slice(&data);
}

unsafe fn write_error(msg: &str) {
    write_resp(&serde_json::json!({ "error": { "message": msg } }));
}
