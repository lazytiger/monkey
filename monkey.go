package gomonkey

/*
#cgo linux  LDFLAGS: -lmozjs185
#cgo darwin LDFLAGS: -lmozjs185

#include "js/jsapi.h"

extern JSClass global_class;
extern JSNative the_go_func_callback;
extern JSErrorReporter the_error_callback;
extern const char* eval_filename;

extern void _JS_SET_RVAL(JSContext *cx, jsval* vp, jsval v);
extern jsval JS_GET_ARGV(JSContext *cx, jsval* vp, int n);

extern jsval GET_JS_NULL();
extern jsval GET_JS_VOID();
*/
import "C"
import "sync"
import "errors"
import "unsafe"
import "reflect"
import "runtime"
import "github.com/realint/monkey/goid"

type ErrorReporter func(report *ErrorReport)

type ErrorReportFlags uint

const (
	JSREPORT_WARNING   = ErrorReportFlags(C.JSREPORT_WARNING)
	JSREPORT_EXCEPTION = ErrorReportFlags(C.JSREPORT_EXCEPTION)
	JSREPORT_STRICT    = ErrorReportFlags(C.JSREPORT_STRICT)
)

type ErrorReport struct {
	Message    string
	FileName   string
	LineBuf    string
	LineNum    int
	ErrorNum   int
	TokenIndex int
	Flags      ErrorReportFlags
}

//export call_error_func
func call_error_func(r unsafe.Pointer, message *C.char, report *C.JSErrorReport) {
	if (*Runtime)(r).errorReporter != nil {
		(*Runtime)(r).errorReporter(&ErrorReport{
			Message:    C.GoString(message),
			FileName:   C.GoString(report.filename),
			LineNum:    int(report.lineno),
			ErrorNum:   int(report.errorNumber),
			LineBuf:    C.GoString(report.linebuf),
			TokenIndex: int(uintptr(unsafe.Pointer(report.tokenptr)) - uintptr(unsafe.Pointer(report.linebuf))),
		})
	}
}

type JsFunc func(argv []Value) (Value, bool)

//export call_go_func
func call_go_func(r unsafe.Pointer, name *C.char, argc C.uintN, vp *C.jsval) C.JSBool {
	var runtime = (*Runtime)(r)

	var argv = make([]Value, int(argc))

	for i := 0; i < len(argv); i++ {
		argv[i] = Value{runtime, C.JS_GET_ARGV(runtime.cx, vp, C.int(i))}
	}

	var result, ok = runtime.callbacks[C.GoString(name)](argv)

	if ok {
		C._JS_SET_RVAL(runtime.cx, vp, result.val)
		return C.JS_TRUE
	}

	return C.JS_FALSE
}

// JavaScript Runtime
type Runtime struct {
	rt            *C.JSRuntime
	cx            *C.JSContext
	global        *C.JSObject
	callbacks     map[string]JsFunc
	errorReporter ErrorReporter
	lockBy        int32
	lockLevel     int
	mutex         sync.Mutex
}

func printCall(argv []Value, newline bool) bool {
	for i := 0; i < len(argv); i++ {
		print(argv[i].ToString())
		if i < len(argv)-1 {
			print(", ")
		}
	}
	if newline {
		println()
	}
	return true
}

// Initializes the JavaScript runtime.
// @maxbytes Maximum number of allocated bytes after which garbage collection is run.
func NewRuntime(maxbytes uint32) (*Runtime, error) {
	r := new(Runtime)
	r.callbacks = make(map[string]JsFunc)

	r.rt = C.JS_NewRuntime(C.uint32(maxbytes))
	if r.rt == nil {
		return nil, errors.New("Could't create JSRuntime")
	}

	r.cx = C.JS_NewContext(r.rt, 8192)
	if r.cx == nil {
		return nil, errors.New("Could't create JSContext")
	}

	C.JS_SetOptions(r.cx, C.JSOPTION_VAROBJFIX|C.JSOPTION_JIT|C.JSOPTION_METHODJIT)
	C.JS_SetVersion(r.cx, C.JSVERSION_LATEST)
	C.JS_SetErrorReporter(r.cx, C.the_error_callback)

	r.global = C.JS_NewCompartmentAndGlobalObject(r.cx, &C.global_class, nil)

	if C.JS_InitStandardClasses(r.cx, r.global) != C.JS_TRUE {
		return nil, errors.New("Could't init global class")
	}

	C.JS_SetRuntimePrivate(r.rt, unsafe.Pointer(r))

	r.DefineFunction("print", func(argv []Value) (Value, bool) {
		return r.Null(), printCall(argv, false)
	})

	r.DefineFunction("println", func(argv []Value) (Value, bool) {
		return r.Null(), printCall(argv, true)
	})

	return r, nil
}

// Frees the JavaScript runtime.
func (r *Runtime) Dispose() {
	C.JS_DestroyContext(r.cx)
	C.JS_DestroyRuntime(r.rt)
}

// Because we can't prevent Go to execute a JavaScript that maybe will execute another JavaScript by invoke Go function.
// Like this: runtime.Eval("eval('1 + 1')")
// So I designed this lock mechanism to let runtime can lock by same goroutine many times.
func (r *Runtime) lock() {
	id := goid.Get()
	if r.lockBy != id {
		r.mutex.Lock()
		r.lockBy = id
	} else {
		r.lockLevel += 1
	}
}

func (r *Runtime) unlock() {
	r.lockLevel -= 1
	if r.lockLevel < 0 {
		r.lockLevel = 0
		r.lockBy = -1
		r.mutex.Unlock()
	}
}

// Set a error reporter
func (r *Runtime) SetErrorReporter(reporter ErrorReporter) {
	r.errorReporter = reporter
}

// Evaluate JavaScript
// When you need high efficiency or run same script many times, please look at Compile() method.
func (r *Runtime) Eval(script string) (Value, bool) {
	r.lock()
	defer r.unlock()

	cscript := C.CString(script)
	defer C.free(unsafe.Pointer(cscript))

	var rval C.jsval
	if C.JS_EvaluateScript(r.cx, r.global, cscript, C.uintN(len(script)), C.eval_filename, 0, &rval) == C.JS_TRUE {
		return Value{r, rval}, true
	}

	return r.Void(), false
}

// Compile JavaScript
// When you need run a script many times, you can use this to avoid dynamic compile.
func (r *Runtime) Compile(script, filename string, lineno int) *Script {
	r.lock()
	defer r.unlock()

	cscript := C.CString(script)
	defer C.free(unsafe.Pointer(cscript))

	cfilename := C.CString(filename)
	defer C.free(unsafe.Pointer(cfilename))

	var scriptObj = C.JS_CompileScript(r.cx, r.global, cscript, C.size_t(len(script)), cfilename, C.uintN(lineno))

	if scriptObj != nil {
		return &Script{r, scriptObj}
	}

	return nil
}

// Define a function into runtime
// @name     The function name
// @callback The function implement
func (r *Runtime) DefineFunction(name string, callback JsFunc) bool {
	r.lock()
	defer r.unlock()

	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	if C.JS_DefineFunction(r.cx, r.global, cname, C.the_go_func_callback, 0, 0) == nil {
		return false
	}

	r.callbacks[name] = callback

	return true
}

// Warp int32
func (r *Runtime) Int(v int32) Value {
	return Value{r, C.INT_TO_JSVAL(C.int32(v))}
}

// Warp null
func (r *Runtime) Null() Value {
	return Value{r, C.GET_JS_NULL()}
}

// Warp void
func (r *Runtime) Void() Value {
	return Value{r, C.GET_JS_VOID()}
}

// Warp boolean
func (r *Runtime) Boolean(v bool) Value {
	if v {
		return Value{r, C.JS_TRUE}
	}
	return Value{r, C.JS_FALSE}
}

// Warp string
func (r *Runtime) String(v string) Value {
	cv := C.CString(v)
	defer C.free(unsafe.Pointer(cv))
	return Value{r, C.STRING_TO_JSVAL(C.JS_NewStringCopyN(r.cx, cv, C.size_t(len(v))))}
}

// Create an empty array, like: []
func (r *Runtime) NewArray() *Object {
	return newObject(r, C.JS_NewArrayObject(r.cx, 0, nil))
}

// Create an empty object, like: {}
func (r *Runtime) NewObject() *Object {
	return newObject(r, C.JS_NewObject(r.cx, nil, nil, nil))
}

// Compiled Script
type Script struct {
	runtime   *Runtime
	scriptObj *C.JSObject
}

// Execute the script
func (s *Script) Execute() (Value, bool) {
	s.runtime.lock()
	defer s.runtime.unlock()

	var rval C.jsval
	if C.JS_ExecuteScript(s.runtime.cx, s.runtime.global, s.scriptObj, &rval) == C.JS_TRUE {
		return Value{s.runtime, rval}, true
	}

	return s.runtime.Void(), false
}

// JavaScript Value
type Value struct {
	rt  *Runtime
	val C.jsval
}

func (v Value) IsNull() bool {
	if C.JSVAL_IS_NULL(v.val) == C.JS_TRUE {
		return true
	}
	return false
}

func (v Value) IsVoid() bool {
	if C.JSVAL_IS_VOID(v.val) == C.JS_TRUE {
		return true
	}
	return false
}

func (v Value) IsInt() bool {
	if C.JSVAL_IS_INT(v.val) == C.JS_TRUE {
		return true
	}
	return false
}

func (v Value) IsString() bool {
	if C.JSVAL_IS_STRING(v.val) == C.JS_TRUE {
		return true
	}
	return false
}

func (v Value) IsNumber() bool {
	if C.JSVAL_IS_NUMBER(v.val) == C.JS_TRUE {
		return true
	}
	return false
}

func (v Value) IsBoolean() bool {
	if C.JSVAL_IS_BOOLEAN(v.val) == C.JS_TRUE {
		return true
	}
	return false
}

func (v Value) IsObject() bool {
	if C.JSVAL_IS_OBJECT(v.val) == C.JS_TRUE {
		return true
	}
	return false
}

func (v Value) IsFunction() bool {
	if v.IsObject() && C.JS_ObjectIsFunction(v.rt.cx, C.JSVAL_TO_OBJECT(v.val)) == C.JS_TRUE {
		return true
	}
	return false
}

// Try convert a value to String.
func (v Value) ToString() string {
	cstring := C.JS_EncodeString(v.rt.cx, C.JS_ValueToString(v.rt.cx, v.val))
	gostring := C.GoString(cstring)
	C.JS_free(v.rt.cx, unsafe.Pointer(cstring))
	return gostring
}

// Try convert a value to Int.
func (v Value) ToInt() (int32, bool) {
	var r C.int32
	if C.JS_ValueToInt32(v.rt.cx, v.val, &r) == C.JS_TRUE {
		return int32(r), true
	}
	return 0, false
}

// Try convert a value to Number.
func (v Value) ToNumber() (float64, bool) {
	var r C.jsdouble
	if C.JS_ValueToNumber(v.rt.cx, v.val, &r) == C.JS_TRUE {
		return float64(r), true
	}
	return 0, false
}

// Try convert a value to Boolean.
func (v Value) ToBoolean() (bool, bool) {
	var r C.JSBool
	if C.JS_ValueToBoolean(v.rt.cx, v.val, &r) == C.JS_TRUE {
		if r == C.JS_TRUE {
			return true, true
		}
		return false, true
	}
	return false, false
}

// Try convert a value to Object.
func (v Value) ToObject() (*Object, bool) {
	var obj *C.JSObject
	if C.JS_ValueToObject(v.rt.cx, v.val, &obj) == C.JS_TRUE {
		return newObject(v.rt, obj), true
	}
	return nil, false
}

// !!! This function will make program fault when the value not a really String.
func (v Value) String() string {
	cstring := C.JS_EncodeString(v.rt.cx, C.JSVAL_TO_STRING(v.val))
	gostring := C.GoString(cstring)
	C.JS_free(v.rt.cx, unsafe.Pointer(cstring))

	return gostring
}

// !!! This function will make program fault when the value not a really Int.
func (v Value) Int() int32 {
	return int32(C.JSVAL_TO_INT(v.val))
}

// !!! This function will make program fault when the value not a really Number.
func (v Value) Number() float64 {
	return float64(C.JSVAL_TO_DOUBLE(v.val))
}

// !!! This function will make program fault when the value not a really Boolean.
func (v Value) Boolean() bool {
	if C.JSVAL_TO_BOOLEAN(v.val) == C.JS_TRUE {
		return true
	}
	return false
}

// !!! This function will make program fault when the value not a really Object.
func (v Value) Object() *Object {
	return newObject(v.rt, C.JSVAL_TO_OBJECT(v.val))
}

func (v Value) Call(argv []Value) (Value, bool) {
	argv2 := make([]C.jsval, len(argv))
	for i := 0; i < len(argv); i++ {
		argv2[i] = argv[i].val
	}
	argv3 := unsafe.Pointer(&argv2)
	argv4 := (*reflect.SliceHeader)(argv3).Data
	argv5 := (*C.jsval)(unsafe.Pointer(argv4))

	r := v.rt.Void()
	if C.JS_CallFunctionValue(v.rt.cx, nil, v.val, C.uintN(len(argv)), argv5, &r.val) == C.JS_TRUE {
		return r, true
	}

	return r, false
}

func (v Value) TypeName() string {
	jstype := C.JS_TypeOfValue(v.rt.cx, v.val)
	return C.GoString(C.JS_GetTypeName(v.rt.cx, jstype))
}

// JavaScript Object
type Object struct {
	rt  *Runtime
	obj *C.JSObject
}

// Add the JSObject to the garbage collector's root set.
// Reference: https://developer.mozilla.org/en-US/docs/Mozilla/Projects/SpiderMonkey/JSAPI_reference/JS_AddRoot
func newObject(rt *Runtime, obj *C.JSObject) *Object {
	result := &Object{rt, obj}

	C.JS_AddObjectRoot(rt.cx, &(result.obj))

	runtime.SetFinalizer(result, func(o *Object) {
		C.JS_RemoveObjectRoot(o.rt.cx, &(o.obj))
	})

	return result
}

func (o *Object) IsArray() bool {
	if C.JS_IsArrayObject(o.rt.cx, o.obj) == C.JS_TRUE {
		return true
	}
	return false
}

func (o *Object) GetArrayLength() int {
	var l C.jsuint
	C.JS_GetArrayLength(o.rt.cx, o.obj, &l)
	return int(l)
}

func (o *Object) SetArrayLength(length int) bool {
	if C.JS_SetArrayLength(o.rt.cx, o.obj, C.jsuint(length)) == C.JS_TRUE {
		return true
	}
	return false
}

func (o *Object) GetElement(index int) (Value, bool) {
	r := o.rt.Void()
	if C.JS_GetElement(o.rt.cx, o.obj, C.jsint(index), &r.val) == C.JS_TRUE {
		return r, true
	}
	return r, false
}

func (o *Object) SetElement(index int, v Value) bool {
	if C.JS_SetElement(o.rt.cx, o.obj, C.jsint(index), &v.val) == C.JS_TRUE {
		return true
	}
	return false
}

func (o *Object) GetProperty(name string) (Value, bool) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	r := o.rt.Void()
	if C.JS_GetProperty(o.rt.cx, o.obj, cname, &r.val) == C.JS_TRUE {
		return r, true
	}
	return r, false
}

func (o *Object) SetProperty(name string, v Value) bool {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	if C.JS_SetProperty(o.rt.cx, o.obj, cname, &v.val) == C.JS_TRUE {
		return true
	}
	return false
}

func (o *Object) ToValue() Value {
	return Value{o.rt, C.OBJECT_TO_JSVAL(o.obj)}
}
