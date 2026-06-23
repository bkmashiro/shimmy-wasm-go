OUT ?= eval.wasm
ZIG ?= zig

.PHONY: wasm clean

wasm:
	$(ZIG) c++ \
		-target wasm32-freestanding \
		-Oz \
		-nostdlib \
		-fno-exceptions \
		-fno-rtti \
		-Iinclude \
		-Wl,--no-entry \
		-Wl,--export=alloc \
		-Wl,--export=evaluate \
		-Wl,--export-memory \
		-Wl,--initial-memory=2097152 \
		-o $(OUT) \
		src/evaluator.cpp src/compare.cpp

clean:
	rm -f eval.wasm
