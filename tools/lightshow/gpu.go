//go:build linux || windows

package main

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// GPU load: short FMA-loop kernels (about 4 ms each) launched back to back for a share of
// every 100 ms, so the reported utilisation follows gpuDuty. NVIDIA through the CUDA driver
// API (PTX, no compiler needed), anything else through OpenCL.

const gpuThreads = 131072

type gpuDev interface {
	Name() string
	Init() error // on the OS thread that will Launch
	SetIters(n uint32)
	Launch() error // one kernel, returns when it has finished
	Close()
}

// keep holds everything whose address is handed to C, so it stays allocated (Go's heap
// objects don't move).
var (
	keepMu sync.Mutex
	keep   []any
)

func pin(v any) { keepMu.Lock(); keep = append(keep, v); keepMu.Unlock() }

func gpuDuty(s float64, k int) float64 {
	s += float64(k) * 23
	p := math.Mod(s, 120)
	switch {
	case p < 40:
		return 0.5 + 0.35*math.Sin(2*math.Pi*p/20)
	case p < 52:
		if math.Mod(p, 4) < 2 {
			return 0.97
		}
		return 0.05
	case p < 70:
		return 0.1 + 0.85*(p-52)/18
	case p < 80:
		return 0
	default:
		return 0.6 + 0.3*math.Sin(2*math.Pi*p/8)
	}
}

func gpuWave(ctx context.Context, start time.Time, st *stats) {
	devs := openCUDA()
	if len(devs) == 0 {
		devs = openOpenCL()
	}
	if len(devs) == 0 {
		logf("gpu: no usable GPU, skipping")
		return
	}
	var wg sync.WaitGroup
	for k, d := range devs {
		wg.Add(1)
		go func(k int, d gpuDev) { defer wg.Done(); gpuLoop(ctx, k, d, start, st) }(k, d)
	}
	wg.Wait()
}

func gpuLoop(ctx context.Context, k int, d gpuDev, start time.Time, st *stats) {
	runtime.LockOSThread() // a CUDA context is current per OS thread
	defer runtime.UnlockOSThread()
	if err := d.Init(); err != nil {
		logf("gpu %s: %v, skipping", d.Name(), err)
		return
	}
	defer d.Close()
	// Each launch is kept near 4 ms: short enough to follow the duty cycle (and far below
	// Windows' 2 s GPU watchdog), long enough that launch overhead doesn't eat the load. The
	// size keeps adapting, because clocks ramp up and down under load: a one-off calibration
	// on an idle GPU can come out ten times too small.
	const target, period, maxIters = 4 * time.Millisecond, 100 * time.Millisecond, uint32(1 << 22)
	iters := uint32(256)
	var spent time.Duration
	launches, ready := 0, false
	for ctx.Err() == nil {
		begin := time.Now()
		u := gpuDuty(begin.Sub(start).Seconds(), k)
		st.setGPU(d.Name(), u)
		d.SetIters(iters)
		for time.Since(begin) < time.Duration(u*float64(period)) {
			t := time.Now()
			if err := d.Launch(); err != nil {
				logf("gpu %s: %v, stopping", d.Name(), err)
				return
			}
			spent += time.Since(t)
			launches++
		}
		if launches >= 8 {
			avg := spent / time.Duration(launches)
			switch {
			case avg < target/2 && iters < maxIters:
				iters = min(iters*2, maxIters)
			case avg > target*2 && iters > 16:
				iters /= 2
			case !ready:
				ready = true
				logf("gpu %s: running, %d iterations per %v launch", d.Name(), iters, avg.Round(100*time.Microsecond))
			}
			spent, launches = 0, 0
		}
		if rest := period - time.Since(begin); rest > 0 {
			time.Sleep(rest)
		}
	}
}

// ---- CUDA driver API ----------------------------------------------------------------------------

const ptxSrc = `.version 6.0
.target sm_50
.address_size 64

.visible .entry burn(
	.param .u64 burn_param_0,
	.param .u32 burn_param_1
)
{
	.reg .pred 	%p<2>;
	.reg .b32 	%r<8>;
	.reg .f32 	%f<8>;
	.reg .b64 	%rd<5>;

	ld.param.u64 	%rd1, [burn_param_0];
	ld.param.u32 	%r1, [burn_param_1];
	mov.u32 	%r2, %ctaid.x;
	mov.u32 	%r3, %ntid.x;
	mov.u32 	%r4, %tid.x;
	mad.lo.s32 	%r5, %r2, %r3, %r4;
	cvt.rn.f32.s32 	%f1, %r5;
	mul.f32 	%f1, %f1, 0f38D1B717;
	mov.f32 	%f2, 0f3F800347;
	mov.f32 	%f3, 0f3F7FF973;
	mov.u32 	%r6, 0;
$L_loop:
	fma.rn.f32 	%f1, %f1, %f2, %f3;
	fma.rn.f32 	%f2, %f2, %f3, %f1;
	fma.rn.f32 	%f3, %f3, %f1, %f2;
	fma.rn.f32 	%f1, %f1, %f2, %f3;
	fma.rn.f32 	%f2, %f2, %f3, %f1;
	fma.rn.f32 	%f3, %f3, %f1, %f2;
	fma.rn.f32 	%f1, %f1, %f2, %f3;
	fma.rn.f32 	%f2, %f2, %f3, %f1;
	add.s32 	%r6, %r6, 1;
	setp.lt.u32 	%p1, %r6, %r1;
	@%p1 bra 	$L_loop;
	add.f32 	%f4, %f1, %f2;
	add.f32 	%f4, %f4, %f3;
	cvta.to.global.u64 	%rd2, %rd1;
	mul.wide.s32 	%rd3, %r5, 4;
	add.s64 	%rd4, %rd2, %rd3;
	st.global.f32 	[%rd4], %f4;
	ret;
}
`

type cudaLib struct {
	init, getCount, get, getName, retain, release, setCurrent, loadData, getFunc, alloc, free, launch, sync, unload uintptr
}

func loadCUDA() (*cudaLib, error) {
	h, err := openLib(cudaLibNames()...)
	if err != nil {
		return nil, err
	}
	l := &cudaLib{}
	for _, s := range []struct {
		p     *uintptr
		names []string
	}{
		{&l.init, []string{"cuInit"}}, {&l.getCount, []string{"cuDeviceGetCount"}}, {&l.get, []string{"cuDeviceGet"}},
		{&l.getName, []string{"cuDeviceGetName"}}, {&l.retain, []string{"cuDevicePrimaryCtxRetain"}},
		{&l.release, []string{"cuDevicePrimaryCtxRelease_v2", "cuDevicePrimaryCtxRelease"}},
		{&l.setCurrent, []string{"cuCtxSetCurrent"}}, {&l.loadData, []string{"cuModuleLoadData"}},
		{&l.getFunc, []string{"cuModuleGetFunction"}}, {&l.alloc, []string{"cuMemAlloc_v2"}}, {&l.free, []string{"cuMemFree_v2"}},
		{&l.launch, []string{"cuLaunchKernel"}}, {&l.sync, []string{"cuCtxSynchronize"}}, {&l.unload, []string{"cuModuleUnload"}},
	} {
		for _, n := range s.names {
			if *s.p, err = sym(h, n); err == nil {
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %v", s.names[0], err)
		}
	}
	return l, nil
}

type cudaDev struct {
	l     *cudaLib
	name  string
	dev   int32
	ctx   uintptr
	mod   uintptr
	fn    uintptr
	dptr  uint64
	iters uint32
	args  [2]uintptr
	ptx   []byte
	fname []byte
}

func openCUDA() []gpuDev {
	l, err := loadCUDA()
	if err != nil {
		logf("gpu: no CUDA (%v)", err)
		return nil
	}
	if r := int32(ccall(l.init, 0)); r != 0 {
		logf("gpu: cuInit failed (%d)", r)
		return nil
	}
	cnt := new(int32)
	pin(cnt)
	if r := int32(ccall(l.getCount, uintptr(unsafe.Pointer(cnt)))); r != 0 || *cnt == 0 {
		logf("gpu: no CUDA devices (%d)", r)
		return nil
	}
	var out []gpuDev
	for i := 0; i < int(*cnt); i++ {
		d := &cudaDev{l: l}
		pin(d)
		if r := int32(ccall(l.get, uintptr(unsafe.Pointer(&d.dev)), uintptr(i))); r != 0 {
			continue
		}
		nb := make([]byte, 256)
		pin(nb)
		ccall(l.getName, uintptr(unsafe.Pointer(&nb[0])), 256, uintptr(d.dev))
		d.name = cstr(nb)
		out = append(out, d)
	}
	return out
}

func (d *cudaDev) Name() string      { return d.name }
func (d *cudaDev) SetIters(n uint32) { d.iters = n }

func (d *cudaDev) Init() error {
	l := d.l
	if r := int32(ccall(l.retain, uintptr(unsafe.Pointer(&d.ctx)), uintptr(d.dev))); r != 0 {
		return fmt.Errorf("cuDevicePrimaryCtxRetain: %d", r)
	}
	if r := int32(ccall(l.setCurrent, d.ctx)); r != 0 {
		return fmt.Errorf("cuCtxSetCurrent: %d", r)
	}
	d.ptx = append([]byte(ptxSrc), 0)
	if r := int32(ccall(l.loadData, uintptr(unsafe.Pointer(&d.mod)), uintptr(unsafe.Pointer(&d.ptx[0])))); r != 0 {
		return fmt.Errorf("cuModuleLoadData: %d", r)
	}
	d.fname = append([]byte("burn"), 0)
	if r := int32(ccall(l.getFunc, uintptr(unsafe.Pointer(&d.fn)), d.mod, uintptr(unsafe.Pointer(&d.fname[0])))); r != 0 {
		return fmt.Errorf("cuModuleGetFunction: %d", r)
	}
	if r := int32(ccall(l.alloc, uintptr(unsafe.Pointer(&d.dptr)), gpuThreads*4)); r != 0 {
		return fmt.Errorf("cuMemAlloc: %d", r)
	}
	d.args[0] = uintptr(unsafe.Pointer(&d.dptr))
	d.args[1] = uintptr(unsafe.Pointer(&d.iters))
	return nil
}

func (d *cudaDev) Launch() error {
	l := d.l
	if r := int32(ccall(l.launch, d.fn, gpuThreads/256, 1, 1, 256, 1, 1, 0, 0, uintptr(unsafe.Pointer(&d.args[0])), 0)); r != 0 {
		return fmt.Errorf("cuLaunchKernel: %d", r)
	}
	if r := int32(ccall(l.sync)); r != 0 {
		return fmt.Errorf("cuCtxSynchronize: %d", r)
	}
	return nil
}

func (d *cudaDev) Close() {
	l := d.l
	if d.dptr != 0 {
		ccall(l.free, uintptr(d.dptr))
	}
	if d.mod != 0 {
		ccall(l.unload, d.mod)
	}
	if d.ctx != 0 {
		ccall(l.release, uintptr(d.dev))
	}
}

// ---- OpenCL -------------------------------------------------------------------------------------

const clSrc = `__kernel void burn(__global float* out, uint iters) {
	uint i = get_global_id(0);
	float a = (float)i * 0.0001f, b = 1.0001f, c = 0.9999f;
	for (uint k = 0; k < iters; k++) {
		a = fma(a, b, c); b = fma(b, c, a); c = fma(c, a, b); a = fma(a, b, c);
		b = fma(b, c, a); c = fma(c, a, b); a = fma(a, b, c); b = fma(b, c, a);
	}
	out[i] = a + b + c;
}
`

const (
	clDeviceTypeGPU   = 1 << 2
	clDeviceName      = 0x102B
	clProgramBuildLog = 0x1183
	clMemWriteOnly    = 1 << 1
)

type clLib struct {
	getPlatformIDs, getDeviceIDs, getDeviceInfo, createContext, createQueue, createProgram, buildProgram, buildInfo,
	createKernel, createBuffer, setArg, enqueueND, finish, relKernel, relProgram, relMem, relQueue, relContext uintptr
}

func loadOpenCL() (*clLib, error) {
	h, err := openLib(openCLLibNames()...)
	if err != nil {
		return nil, err
	}
	l := &clLib{}
	for _, s := range []struct {
		p *uintptr
		n string
	}{
		{&l.getPlatformIDs, "clGetPlatformIDs"}, {&l.getDeviceIDs, "clGetDeviceIDs"}, {&l.getDeviceInfo, "clGetDeviceInfo"},
		{&l.createContext, "clCreateContext"}, {&l.createQueue, "clCreateCommandQueue"},
		{&l.createProgram, "clCreateProgramWithSource"}, {&l.buildProgram, "clBuildProgram"},
		{&l.buildInfo, "clGetProgramBuildInfo"}, {&l.createKernel, "clCreateKernel"}, {&l.createBuffer, "clCreateBuffer"},
		{&l.setArg, "clSetKernelArg"}, {&l.enqueueND, "clEnqueueNDRangeKernel"}, {&l.finish, "clFinish"},
		{&l.relKernel, "clReleaseKernel"}, {&l.relProgram, "clReleaseProgram"}, {&l.relMem, "clReleaseMemObject"},
		{&l.relQueue, "clReleaseCommandQueue"}, {&l.relContext, "clReleaseContext"},
	} {
		if *s.p, err = sym(h, s.n); err != nil {
			return nil, fmt.Errorf("%s: %v", s.n, err)
		}
	}
	return l, nil
}

type clDev struct {
	l                             *clLib
	id                            uintptr
	name                          string
	ctx, queue, prog, kernel, mem uintptr
	err                           int32
	iters                         uint32
	global                        uintptr
	devs                          [1]uintptr
	src                           []byte
	srcPtr                        uintptr
	kname                         []byte
	log                           []byte
	logLen                        uintptr
}

type clEnum struct {
	n     uint32
	nd    uint32
	plats [16]uintptr
	devs  [16]uintptr
	name  [256]byte
	ret   uintptr
}

func openOpenCL() []gpuDev {
	l, err := loadOpenCL()
	if err != nil {
		logf("gpu: no OpenCL (%v)", err)
		return nil
	}
	e := &clEnum{}
	pin(e)
	if r := int32(ccall(l.getPlatformIDs, 16, uintptr(unsafe.Pointer(&e.plats[0])), uintptr(unsafe.Pointer(&e.n)))); r != 0 || e.n == 0 {
		logf("gpu: no OpenCL platforms (%d)", r)
		return nil
	}
	var out []gpuDev
	for p := 0; p < int(min(e.n, 16)); p++ {
		e.nd = 0
		if r := int32(ccall(l.getDeviceIDs, e.plats[p], clDeviceTypeGPU, 16, uintptr(unsafe.Pointer(&e.devs[0])), uintptr(unsafe.Pointer(&e.nd)))); r != 0 {
			continue
		}
		for k := 0; k < int(min(e.nd, 16)); k++ {
			e.name = [256]byte{}
			ccall(l.getDeviceInfo, e.devs[k], clDeviceName, 256, uintptr(unsafe.Pointer(&e.name[0])), uintptr(unsafe.Pointer(&e.ret)))
			d := &clDev{l: l, id: e.devs[k], name: cstr(e.name[:])}
			pin(d)
			out = append(out, d)
		}
	}
	return out
}

func (d *clDev) Name() string      { return d.name }
func (d *clDev) SetIters(n uint32) { d.iters = n }

func (d *clDev) Init() error {
	l := d.l
	d.devs[0] = d.id
	d.ctx = ccall(l.createContext, 0, 1, uintptr(unsafe.Pointer(&d.devs[0])), 0, 0, uintptr(unsafe.Pointer(&d.err)))
	if d.err != 0 || d.ctx == 0 {
		return fmt.Errorf("clCreateContext: %d", d.err)
	}
	d.queue = ccall(l.createQueue, d.ctx, d.id, 0, uintptr(unsafe.Pointer(&d.err)))
	if d.err != 0 || d.queue == 0 {
		return fmt.Errorf("clCreateCommandQueue: %d", d.err)
	}
	d.src = append([]byte(clSrc), 0)
	d.srcPtr = uintptr(unsafe.Pointer(&d.src[0]))
	d.prog = ccall(l.createProgram, d.ctx, 1, uintptr(unsafe.Pointer(&d.srcPtr)), 0, uintptr(unsafe.Pointer(&d.err)))
	if d.err != 0 || d.prog == 0 {
		return fmt.Errorf("clCreateProgramWithSource: %d", d.err)
	}
	if r := int32(ccall(l.buildProgram, d.prog, 1, uintptr(unsafe.Pointer(&d.devs[0])), 0, 0, 0)); r != 0 {
		d.log = make([]byte, 4096)
		ccall(l.buildInfo, d.prog, d.id, clProgramBuildLog, 4096, uintptr(unsafe.Pointer(&d.log[0])), uintptr(unsafe.Pointer(&d.logLen)))
		return fmt.Errorf("clBuildProgram: %d: %s", r, cstr(d.log))
	}
	d.kname = append([]byte("burn"), 0)
	d.kernel = ccall(l.createKernel, d.prog, uintptr(unsafe.Pointer(&d.kname[0])), uintptr(unsafe.Pointer(&d.err)))
	if d.err != 0 || d.kernel == 0 {
		return fmt.Errorf("clCreateKernel: %d", d.err)
	}
	d.mem = ccall(l.createBuffer, d.ctx, clMemWriteOnly, gpuThreads*4, 0, uintptr(unsafe.Pointer(&d.err)))
	if d.err != 0 || d.mem == 0 {
		return fmt.Errorf("clCreateBuffer: %d", d.err)
	}
	if r := int32(ccall(l.setArg, d.kernel, 0, unsafe.Sizeof(d.mem), uintptr(unsafe.Pointer(&d.mem)))); r != 0 {
		return fmt.Errorf("clSetKernelArg 0: %d", r)
	}
	d.global = gpuThreads
	return nil
}

func (d *clDev) Launch() error {
	l := d.l
	if r := int32(ccall(l.setArg, d.kernel, 1, 4, uintptr(unsafe.Pointer(&d.iters)))); r != 0 {
		return fmt.Errorf("clSetKernelArg 1: %d", r)
	}
	if r := int32(ccall(l.enqueueND, d.queue, d.kernel, 1, 0, uintptr(unsafe.Pointer(&d.global)), 0, 0, 0, 0)); r != 0 {
		return fmt.Errorf("clEnqueueNDRangeKernel: %d", r)
	}
	if r := int32(ccall(l.finish, d.queue)); r != 0 {
		return fmt.Errorf("clFinish: %d", r)
	}
	return nil
}

func (d *clDev) Close() {
	l := d.l
	for _, x := range []struct{ fn, h uintptr }{{l.relKernel, d.kernel}, {l.relMem, d.mem}, {l.relProgram, d.prog}, {l.relQueue, d.queue}, {l.relContext, d.ctx}} {
		if x.h != 0 {
			ccall(x.fn, x.h)
		}
	}
}
