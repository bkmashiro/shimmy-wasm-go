#![no_std]
#![no_main]

mod compare;

use core::panic::PanicInfo;

static mut REQ_BUF: [u8; 256 * 1024] = [0; 256 * 1024];
static mut RESP_BUF: [u8; 256 * 1024] = [0; 256 * 1024];
static mut INVOCATION_COUNT: u32 = 0;

#[panic_handler]
fn panic(_info: &PanicInfo) -> ! {
    loop {}
}

#[no_mangle]
pub extern "C" fn alloc(_size: i32) -> i32 {
    core::ptr::addr_of_mut!(REQ_BUF) as *mut u8 as i32
}

#[no_mangle]
pub extern "C" fn evaluate(_req_ptr: i32, req_len: i32) -> i32 {
    unsafe {
        INVOCATION_COUNT += 1;
    }

    let req = unsafe {
        core::slice::from_raw_parts(core::ptr::addr_of!(REQ_BUF) as *const u8, req_len as usize)
    };

    let response = json_string_field(req, b"response");
    let answer = json_string_field(req, b"answer");
    let correct_feedback = json_string_field(req, b"correct_response_feedback");
    let incorrect_feedback = json_string_field(req, b"incorrect_response_feedback");

    let is_correct = compare::is_correct(response, answer);
    let feedback = compare::feedback(is_correct, correct_feedback, incorrect_feedback);
    let count = unsafe { INVOCATION_COUNT };

    write_response(is_correct, feedback, count)
}

fn json_string_field<'a>(input: &'a [u8], name: &[u8]) -> Option<&'a [u8]> {
    let key = find_key(input, name)?;
    let mut i = key + name.len() + 2;
    while i < input.len()
        && (input[i] == b' ' || input[i] == b'\n' || input[i] == b'\t' || input[i] == b'\r')
    {
        i += 1;
    }
    if i >= input.len() || input[i] != b':' {
        return None;
    }
    i += 1;
    while i < input.len()
        && (input[i] == b' ' || input[i] == b'\n' || input[i] == b'\t' || input[i] == b'\r')
    {
        i += 1;
    }
    if i >= input.len() || input[i] != b'"' {
        return None;
    }
    i += 1;
    let start = i;
    while i < input.len() {
        match input[i] {
            b'"' => return Some(&input[start..i]),
            b'\\' => i += 2,
            _ => i += 1,
        }
    }
    None
}

fn find_key(input: &[u8], name: &[u8]) -> Option<usize> {
    if input.len() < name.len() + 2 {
        return None;
    }
    let last = input.len() - name.len() - 1;
    let mut i = 0;
    while i < last {
        if input[i] == b'"'
            && &input[i + 1..i + 1 + name.len()] == name
            && input[i + 1 + name.len()] == b'"'
        {
            return Some(i);
        }
        i += 1;
    }
    None
}

fn write_response(is_correct: bool, feedback: &[u8], count: u32) -> i32 {
    let mut w = Writer::new();
    w.bytes(b"{\"command\":\"eval\",\"result\":{\"is_correct\":");
    w.bytes(if is_correct { b"true" } else { b"false" });
    w.bytes(b",\"feedback\":\"");
    w.json_string_bytes(feedback);
    w.bytes(b"\",\"guest_invocation_count\":");
    w.u32(count);
    w.bytes(b",\"snapshot_isolation_ok\":");
    w.bytes(if count == 1 { b"true" } else { b"false" });
    w.bytes(b"}}");
    w.finish()
}

struct Writer {
    pos: usize,
}

impl Writer {
    fn new() -> Self {
        Self { pos: 4 }
    }

    fn bytes(&mut self, bytes: &[u8]) {
        for &b in bytes {
            self.push(b);
        }
    }

    fn json_string_bytes(&mut self, bytes: &[u8]) {
        for &b in bytes {
            match b {
                b'"' => self.bytes(b"\\\""),
                b'\\' => self.bytes(b"\\\\"),
                _ => self.push(b),
            }
        }
    }

    fn u32(&mut self, mut n: u32) {
        if n == 0 {
            self.push(b'0');
            return;
        }
        let mut digits = [0u8; 10];
        let mut len = 0;
        while n > 0 {
            digits[len] = b'0' + (n % 10) as u8;
            n /= 10;
            len += 1;
        }
        while len > 0 {
            len -= 1;
            self.push(digits[len]);
        }
    }

    fn push(&mut self, b: u8) {
        unsafe {
            let ptr = core::ptr::addr_of_mut!(RESP_BUF) as *mut u8;
            *ptr.add(self.pos) = b;
        }
        self.pos += 1;
    }

    fn finish(self) -> i32 {
        let len = (self.pos - 4) as u32;
        unsafe {
            let ptr = core::ptr::addr_of_mut!(RESP_BUF) as *mut u8;
            *ptr.add(0) = (len & 0xff) as u8;
            *ptr.add(1) = ((len >> 8) & 0xff) as u8;
            *ptr.add(2) = ((len >> 16) & 0xff) as u8;
            *ptr.add(3) = ((len >> 24) & 0xff) as u8;
            ptr as i32
        }
    }
}
