WASI_SDK ?= /opt/wasi-sdk
CC        := $(WASI_SDK)/bin/clang
CFLAGS    := --target=wasm32-wasip1 \
             --sysroot=$(WASI_SDK)/share/wasi-sysroot \
             -O2 -Wall \
             -Wl,--export=alloc \
             -Wl,--export=dispatch \
             -Wl,--no-entry

.PHONY: all clean

all: eval.wasm

eval.wasm: eval.c jsmn.h
	$(CC) $(CFLAGS) -o $@ eval.c

clean:
	rm -f eval.wasm
