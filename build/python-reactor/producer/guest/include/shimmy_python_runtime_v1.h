#ifndef SHIMMY_PYTHON_RUNTIME_V1_H
#define SHIMMY_PYTHON_RUNTIME_V1_H

#include <stdint.h>

#define SHIMMY_PYTHON_RUNTIME_IDENTITY 0x53505231u
#define SHIMMY_REQUEST_MAX_BYTES (1u << 20)
#define SHIMMY_RESPONSE_MAX_BYTES (1u << 20)
#define SHIMMY_RESPONSE_PREFIX_BYTES 4u

uint32_t shimmy_python_runtime_identity(void);
int32_t shimmy_python_init(void);
int32_t shimmy_python_prepare(uint32_t source_ptr, uint32_t source_len);
uint32_t alloc(uint32_t size);
void dealloc(uint32_t ptr);
uint32_t evaluate(uint32_t request_ptr, uint32_t request_len);

#endif
