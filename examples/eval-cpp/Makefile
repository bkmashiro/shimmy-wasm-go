WASI_SDK ?= /opt/wasi-sdk
CXX       := $(WASI_SDK)/bin/clang++
CXXFLAGS  := --target=wasm32-wasip1 \
             --sysroot=$(WASI_SDK)/share/wasi-sysroot \
             -std=c++17 -O2 -Wall \
             -Wl,--export=alloc \
             -Wl,--export=dispatch \
             -Wl,--no-entry

.PHONY: all clean

all: eval.wasm

eval.wasm: eval.cpp
	$(CXX) $(CXXFLAGS) -o $@ eval.cpp

clean:
	rm -f eval.wasm
