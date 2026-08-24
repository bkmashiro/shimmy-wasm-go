#include "shimmy_python_runtime_v1.h"

#include <Python.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "shimmy_python_bootstrap.inc"

#ifdef SHIMMY_NUMPY_CORE
PyMODINIT_FUNC PyInit__multiarray_umath(void);
PyMODINIT_FUNC PyInit__umath_linalg(void);
#endif

static unsigned char shimmy_response[
    SHIMMY_RESPONSE_PREFIX_BYTES + SHIMMY_RESPONSE_MAX_BYTES
];
static PyObject *shimmy_prepare_callable = NULL;
static PyObject *shimmy_handle_callable = NULL;
static int shimmy_initialized = 0;
static int shimmy_prepared = 0;

static void write_u32_le(unsigned char *target, uint32_t value) {
    target[0] = (unsigned char)(value & 0xffu);
    target[1] = (unsigned char)((value >> 8) & 0xffu);
    target[2] = (unsigned char)((value >> 16) & 0xffu);
    target[3] = (unsigned char)((value >> 24) & 0xffu);
}

static uint32_t response_pointer(void) {
    return (uint32_t)(uintptr_t)shimmy_response;
}

static uint32_t write_response(const char *bytes, size_t length) {
    if (bytes == NULL || length > SHIMMY_RESPONSE_MAX_BYTES) {
        static const char too_large[] =
            "{\"status\":\"error\",\"error\":{\"type\":\"ResponseTooLarge\","
            "\"message\":\"response exceeds configured limit\"}}";
        bytes = too_large;
        length = sizeof(too_large) - 1u;
    }
    write_u32_le(shimmy_response, (uint32_t)length);
    memcpy(shimmy_response + SHIMMY_RESPONSE_PREFIX_BYTES, bytes, length);
    return response_pointer();
}

static uint32_t write_fixed_error(const char *type, const char *message) {
    char buffer[512];
    int length = snprintf(
        buffer,
        sizeof(buffer),
        "{\"status\":\"error\",\"error\":{\"type\":\"%s\",\"message\":\"%s\"}}",
        type,
        message
    );
    if (length < 0 || (size_t)length >= sizeof(buffer)) {
        static const char fallback[] =
            "{\"status\":\"error\",\"error\":{\"type\":\"RuntimeError\","
            "\"message\":\"guest error encoding failed\"}}";
        return write_response(fallback, sizeof(fallback) - 1u);
    }
    return write_response(buffer, (size_t)length);
}

__attribute__((export_name("shimmy_python_runtime_identity")))
uint32_t shimmy_python_runtime_identity(void) {
    return SHIMMY_PYTHON_RUNTIME_IDENTITY;
}

__attribute__((export_name("shimmy_python_init")))
int32_t shimmy_python_init(void) {
    if (shimmy_initialized) {
        return 0;
    }

#ifdef SHIMMY_NUMPY_CORE
    if (PyImport_AppendInittab("numpy._core._multiarray_umath", PyInit__multiarray_umath) < 0) {
        return -6;
    }
    if (PyImport_AppendInittab("numpy.linalg._umath_linalg", PyInit__umath_linalg) < 0) {
        return -7;
    }
#endif

    PyConfig config;
    PyStatus status;
    PyConfig_InitIsolatedConfig(&config);
    config.use_environment = 0;
    config.user_site_directory = 0;
    config.site_import = 0;
    config.write_bytecode = 0;
    config.install_signal_handlers = 0;
    config.parse_argv = 0;
    config.module_search_paths_set = 1;

    status = PyConfig_SetString(&config, &config.program_name, L"shimmy-python");
    if (!PyStatus_Exception(status)) {
        status = PyConfig_SetString(&config, &config.home, L"/usr/local");
    }
    if (!PyStatus_Exception(status)) {
        status = PyWideStringList_Append(
            &config.module_search_paths,
            L"/usr/local/lib/python3.14"
        );
    }
    if (!PyStatus_Exception(status)) {
        status = PyWideStringList_Append(
            &config.module_search_paths,
            L"/usr/local/lib/python3.14/site-packages"
        );
    }
    if (!PyStatus_Exception(status)) {
        status = Py_InitializeFromConfig(&config);
    }
    PyConfig_Clear(&config);
    if (PyStatus_Exception(status)) {
        return -1;
    }

    PyObject *main_module = PyImport_AddModule("__main__");
    if (main_module == NULL) {
        PyErr_Print();
        return -2;
    }
    PyObject *globals = PyModule_GetDict(main_module);
    PyObject *compiled = Py_CompileStringExFlags(
        (const char *)shimmy_python_bootstrap,
        "<shimmy-bootstrap>",
        Py_file_input,
        NULL,
        -1
    );
    if (compiled == NULL) {
        PyErr_Print();
        return -3;
    }
    PyObject *executed = PyEval_EvalCode(compiled, globals, globals);
    Py_DECREF(compiled);
    if (executed == NULL) {
        PyErr_Print();
        return -4;
    }
    Py_DECREF(executed);

    shimmy_prepare_callable = PyDict_GetItemString(globals, "_shimmy_prepare");
    shimmy_handle_callable = PyDict_GetItemString(globals, "_shimmy_handle_request");
    if (shimmy_prepare_callable == NULL || shimmy_handle_callable == NULL ||
        !PyCallable_Check(shimmy_prepare_callable) ||
        !PyCallable_Check(shimmy_handle_callable)) {
        return -5;
    }
    Py_INCREF(shimmy_prepare_callable);
    Py_INCREF(shimmy_handle_callable);
    shimmy_initialized = 1;
    return 0;
}

__attribute__((export_name("shimmy_python_prepare")))
int32_t shimmy_python_prepare(uint32_t source_ptr, uint32_t source_len) {
    if (!shimmy_initialized || shimmy_prepare_callable == NULL) {
        return -1;
    }
    if (shimmy_prepared) {
        return -2;
    }
    if (source_ptr == 0 || source_len == 0 || source_len > SHIMMY_REQUEST_MAX_BYTES) {
        return -3;
    }

    const char *source = (const char *)(uintptr_t)source_ptr;
    PyObject *py_source = PyUnicode_DecodeUTF8(source, (Py_ssize_t)source_len, "strict");
    if (py_source == NULL) {
        PyErr_Print();
        return -4;
    }
    PyObject *result = PyObject_CallOneArg(shimmy_prepare_callable, py_source);
    Py_DECREF(py_source);
    if (result == NULL) {
        PyErr_Print();
        return -5;
    }
    Py_DECREF(result);
    shimmy_prepared = 1;
    return 0;
}

__attribute__((export_name("alloc")))
uint32_t alloc(uint32_t size) {
    if (size == 0 || size > SHIMMY_REQUEST_MAX_BYTES) {
        return 0;
    }
    return (uint32_t)(uintptr_t)malloc((size_t)size);
}

__attribute__((export_name("dealloc")))
void dealloc(uint32_t ptr) {
    if (ptr != 0) {
        free((void *)(uintptr_t)ptr);
    }
}

__attribute__((export_name("evaluate")))
uint32_t evaluate(uint32_t request_ptr, uint32_t request_len) {
    if (!shimmy_initialized || !shimmy_prepared || shimmy_handle_callable == NULL) {
        return write_fixed_error("RuntimeError", "guest is not prepared");
    }
    if (request_ptr == 0 || request_len == 0 ||
        request_len > SHIMMY_REQUEST_MAX_BYTES) {
        return write_fixed_error("InvalidRequest", "request size is invalid");
    }

    const char *request = (const char *)(uintptr_t)request_ptr;
    PyObject *py_request = PyUnicode_DecodeUTF8(
        request,
        (Py_ssize_t)request_len,
        "strict"
    );
    if (py_request == NULL) {
        PyErr_Clear();
        return write_fixed_error("InvalidRequest", "request must be UTF-8");
    }
    PyObject *result = PyObject_CallOneArg(shimmy_handle_callable, py_request);
    Py_DECREF(py_request);
    if (result == NULL) {
        PyErr_Print();
        return write_fixed_error("RuntimeError", "request handler failed");
    }

    Py_ssize_t output_len = 0;
    const char *output = PyUnicode_AsUTF8AndSize(result, &output_len);
    uint32_t pointer;
    if (output == NULL || output_len < 0) {
        PyErr_Clear();
        pointer = write_fixed_error("RuntimeError", "response encoding failed");
    } else {
        pointer = write_response(output, (size_t)output_len);
    }
    Py_DECREF(result);
    return pointer;
}
