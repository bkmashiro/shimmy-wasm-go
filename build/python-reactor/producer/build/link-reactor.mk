# Included after CPython's generated WASI Makefile.

.PHONY: shimmy-python-runtime

SHIMMY_RUNTIME_CFLAGS :=
SHIMMY_LINKER := $(LINKCC)
SHIMMY_NUMPY_LINK :=
SHIMMY_NUMPY_LIBC_LINK :=
SHIMMY_CXX_LIBS :=
ifneq ($(strip $(SHIMMY_NUMPY_ARCHIVES)),)
SHIMMY_RUNTIME_CFLAGS += -DSHIMMY_NUMPY_CORE=1
SHIMMY_LINKER := $(CXX)
SHIMMY_NUMPY_LINK := -Wl,--whole-archive $(SHIMMY_NUMPY_ARCHIVES) -Wl,--no-whole-archive
SHIMMY_NUMPY_LIBC_LINK := -lc-printscan-long-double
SHIMMY_CXX_LIBS := -lc++ -lc++abi
endif

shimmy-python-runtime:
	@test -n "$(SHIMMY_RUNTIME_SOURCE)"
	@test -n "$(SHIMMY_RUNTIME_INCLUDE)"
	@test -n "$(SHIMMY_GENERATED_INCLUDE)"
	@test -n "$(SHIMMY_WASI_VFS_LIBRARY)"
	@test -n "$(SHIMMY_OUTPUT)"
	$(CC) $(PY_CORE_CFLAGS) \
		$(SHIMMY_RUNTIME_CFLAGS) \
		-I$(SHIMMY_RUNTIME_INCLUDE) \
		-I$(SHIMMY_GENERATED_INCLUDE) \
		-c $(SHIMMY_RUNTIME_SOURCE) \
		-o shimmy_python_runtime.o
	$(SHIMMY_LINKER) $(PY_CORE_LDFLAGS) $(LINKFORSHARED) \
		-mexec-model=reactor \
		-Wl,-z,stack-size=16777216 \
		-Wl,--stack-first \
		-Wl,--initial-memory=268435456 \
		-Wl,--max-memory=2147483648 \
		-Wl,--export-memory \
		-Wl,--export=shimmy_python_runtime_identity \
		-Wl,--export=shimmy_python_init \
		-Wl,--export=shimmy_python_prepare \
		-Wl,--export=alloc \
		-Wl,--export=dealloc \
		-Wl,--export=evaluate \
		-o $(SHIMMY_OUTPUT) \
		shimmy_python_runtime.o \
		$(SHIMMY_NUMPY_LINK) \
		$(SHIMMY_NUMPY_LIBC_LINK) \
		$(BLDLIBRARY) $(LIBS) $(MODLIBS) $(SYSLIBS) \
		$(SHIMMY_WASI_VFS_LIBRARY) \
		$(SHIMMY_CXX_LIBS)
