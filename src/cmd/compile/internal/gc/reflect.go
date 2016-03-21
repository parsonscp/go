// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"cmd/internal/gcprog"
	"cmd/internal/obj"
	"fmt"
	"os"
	"sort"
	"strings"
)

// runtime interface and reflection data structures
var signatlist []*Node

// byMethodNameAndPackagePath sorts method signatures by name, then package path.
type byMethodNameAndPackagePath []*Sig

func (x byMethodNameAndPackagePath) Len() int      { return len(x) }
func (x byMethodNameAndPackagePath) Swap(i, j int) { x[i], x[j] = x[j], x[i] }
func (x byMethodNameAndPackagePath) Less(i, j int) bool {
	return siglt(x[i], x[j])
}

// siglt reports whether a < b
func siglt(a, b *Sig) bool {
	if a.name != b.name {
		return a.name < b.name
	}
	if a.pkg == b.pkg {
		return false
	}
	if a.pkg == nil {
		return true
	}
	if b.pkg == nil {
		return false
	}
	return a.pkg.Path < b.pkg.Path
}

// Builds a type representing a Bucket structure for
// the given map type. This type is not visible to users -
// we include only enough information to generate a correct GC
// program for it.
// Make sure this stays in sync with ../../../../runtime/hashmap.go!
const (
	BUCKETSIZE = 8
	MAXKEYSIZE = 128
	MAXVALSIZE = 128
)

func structfieldSize() int       { return 3 * Widthptr } // Sizeof(runtime.structfield{})
func imethodSize() int           { return 2 * Widthptr } // Sizeof(runtime.imethod{})
func uncommonSize(t *Type) int { // Sizeof(runtime.uncommontype{})
	if t.Sym == nil && len(methods(t)) == 0 {
		return 0
	}
	return 2*Widthptr + 2*Widthint
}

func makefield(name string, t *Type) *Field {
	f := newField()
	f.Type = t
	f.Sym = nopkg.Lookup(name)
	return f
}

func mapbucket(t *Type) *Type {
	if t.Bucket != nil {
		return t.Bucket
	}

	bucket := typ(TSTRUCT)
	keytype := t.Key()
	valtype := t.Type
	dowidth(keytype)
	dowidth(valtype)
	if keytype.Width > MAXKEYSIZE {
		keytype = Ptrto(keytype)
	}
	if valtype.Width > MAXVALSIZE {
		valtype = Ptrto(valtype)
	}

	// The first field is: uint8 topbits[BUCKETSIZE].
	arr := typ(TARRAY)

	arr.Type = Types[TUINT8]
	arr.Bound = BUCKETSIZE
	field := make([]*Field, 0, 5)
	field = append(field, makefield("topbits", arr))
	arr = typ(TARRAY)
	arr.Type = keytype
	arr.Bound = BUCKETSIZE
	field = append(field, makefield("keys", arr))
	arr = typ(TARRAY)
	arr.Type = valtype
	arr.Bound = BUCKETSIZE
	field = append(field, makefield("values", arr))

	// Make sure the overflow pointer is the last memory in the struct,
	// because the runtime assumes it can use size-ptrSize as the
	// offset of the overflow pointer. We double-check that property
	// below once the offsets and size are computed.
	//
	// BUCKETSIZE is 8, so the struct is aligned to 64 bits to this point.
	// On 32-bit systems, the max alignment is 32-bit, and the
	// overflow pointer will add another 32-bit field, and the struct
	// will end with no padding.
	// On 64-bit systems, the max alignment is 64-bit, and the
	// overflow pointer will add another 64-bit field, and the struct
	// will end with no padding.
	// On nacl/amd64p32, however, the max alignment is 64-bit,
	// but the overflow pointer will add only a 32-bit field,
	// so if the struct needs 64-bit padding (because a key or value does)
	// then it would end with an extra 32-bit padding field.
	// Preempt that by emitting the padding here.
	if int(t.Type.Align) > Widthptr || int(t.Key().Align) > Widthptr {
		field = append(field, makefield("pad", Types[TUINTPTR]))
	}

	// If keys and values have no pointers, the map implementation
	// can keep a list of overflow pointers on the side so that
	// buckets can be marked as having no pointers.
	// Arrange for the bucket to have no pointers by changing
	// the type of the overflow field to uintptr in this case.
	// See comment on hmap.overflow in ../../../../runtime/hashmap.go.
	otyp := Ptrto(bucket)
	if !haspointers(t.Type) && !haspointers(t.Key()) && t.Type.Width <= MAXVALSIZE && t.Key().Width <= MAXKEYSIZE {
		otyp = Types[TUINTPTR]
	}
	ovf := makefield("overflow", otyp)
	field = append(field, ovf)

	// link up fields
	bucket.Noalg = true
	bucket.Local = t.Local
	bucket.SetFields(field[:])
	dowidth(bucket)

	// Double-check that overflow field is final memory in struct,
	// with no padding at end. See comment above.
	if ovf.Width != bucket.Width-int64(Widthptr) {
		Yyerror("bad math in mapbucket for %v", t)
	}

	t.Bucket = bucket

	bucket.Map = t
	return bucket
}

// Builds a type representing a Hmap structure for the given map type.
// Make sure this stays in sync with ../../../../runtime/hashmap.go!
func hmap(t *Type) *Type {
	if t.Hmap != nil {
		return t.Hmap
	}

	bucket := mapbucket(t)
	var field [8]*Field
	field[0] = makefield("count", Types[TINT])
	field[1] = makefield("flags", Types[TUINT8])
	field[2] = makefield("B", Types[TUINT8])
	field[3] = makefield("hash0", Types[TUINT32])
	field[4] = makefield("buckets", Ptrto(bucket))
	field[5] = makefield("oldbuckets", Ptrto(bucket))
	field[6] = makefield("nevacuate", Types[TUINTPTR])
	field[7] = makefield("overflow", Types[TUNSAFEPTR])

	h := typ(TSTRUCT)
	h.Noalg = true
	h.Local = t.Local
	h.SetFields(field[:])
	dowidth(h)
	t.Hmap = h
	h.Map = t
	return h
}

func hiter(t *Type) *Type {
	if t.Hiter != nil {
		return t.Hiter
	}

	// build a struct:
	// hiter {
	//    key *Key
	//    val *Value
	//    t *MapType
	//    h *Hmap
	//    buckets *Bucket
	//    bptr *Bucket
	//    overflow0 unsafe.Pointer
	//    overflow1 unsafe.Pointer
	//    startBucket uintptr
	//    stuff uintptr
	//    bucket uintptr
	//    checkBucket uintptr
	// }
	// must match ../../../../runtime/hashmap.go:hiter.
	var field [12]*Field
	field[0] = makefield("key", Ptrto(t.Key()))
	field[1] = makefield("val", Ptrto(t.Type))
	field[2] = makefield("t", Ptrto(Types[TUINT8]))
	field[3] = makefield("h", Ptrto(hmap(t)))
	field[4] = makefield("buckets", Ptrto(mapbucket(t)))
	field[5] = makefield("bptr", Ptrto(mapbucket(t)))
	field[6] = makefield("overflow0", Types[TUNSAFEPTR])
	field[7] = makefield("overflow1", Types[TUNSAFEPTR])
	field[8] = makefield("startBucket", Types[TUINTPTR])
	field[9] = makefield("stuff", Types[TUINTPTR]) // offset+wrapped+B+I
	field[10] = makefield("bucket", Types[TUINTPTR])
	field[11] = makefield("checkBucket", Types[TUINTPTR])

	// build iterator struct holding the above fields
	i := typ(TSTRUCT)
	i.Noalg = true
	i.SetFields(field[:])
	dowidth(i)
	if i.Width != int64(12*Widthptr) {
		Yyerror("hash_iter size not correct %d %d", i.Width, 12*Widthptr)
	}
	t.Hiter = i
	i.Map = t
	return i
}

// f is method type, with receiver.
// return function type, receiver as first argument (or not).
func methodfunc(f *Type, receiver *Type) *Type {
	var in []*Node
	if receiver != nil {
		d := Nod(ODCLFIELD, nil, nil)
		d.Type = receiver
		in = append(in, d)
	}

	var d *Node
	for _, t := range f.Params().Fields().Slice() {
		d = Nod(ODCLFIELD, nil, nil)
		d.Type = t.Type
		d.Isddd = t.Isddd
		in = append(in, d)
	}

	var out []*Node
	for _, t := range f.Results().Fields().Slice() {
		d = Nod(ODCLFIELD, nil, nil)
		d.Type = t.Type
		out = append(out, d)
	}

	t := functype(nil, in, out)
	if f.Nname != nil {
		// Link to name of original method function.
		t.Nname = f.Nname
	}

	return t
}

// methods returns the methods of the non-interface type t, sorted by name.
// Generates stub functions as needed.
func methods(t *Type) []*Sig {
	// method type
	mt := methtype(t, 0)

	if mt == nil {
		return nil
	}
	expandmeth(mt)

	// type stored in interface word
	it := t

	if !isdirectiface(it) {
		it = Ptrto(t)
	}

	// make list of methods for t,
	// generating code if necessary.
	var ms []*Sig
	for _, f := range mt.AllMethods().Slice() {
		if f.Type.Etype != TFUNC || f.Type.Recv() == nil {
			Fatalf("non-method on %v method %v %v\n", mt, f.Sym, f)
		}
		if f.Type.Recv() == nil {
			Fatalf("receiver with no type on %v method %v %v\n", mt, f.Sym, f)
		}
		if f.Nointerface {
			continue
		}

		method := f.Sym
		if method == nil {
			continue
		}

		// get receiver type for this particular method.
		// if pointer receiver but non-pointer t and
		// this is not an embedded pointer inside a struct,
		// method does not apply.
		this := f.Type.Recv().Type

		if Isptr[this.Etype] && this.Type == t {
			continue
		}
		if Isptr[this.Etype] && !Isptr[t.Etype] && f.Embedded != 2 && !isifacemethod(f.Type) {
			continue
		}

		var sig Sig
		ms = append(ms, &sig)

		sig.name = method.Name
		if !exportname(method.Name) {
			if method.Pkg == nil {
				Fatalf("methods: missing package")
			}
			sig.pkg = method.Pkg
		}

		sig.isym = methodsym(method, it, 1)
		sig.tsym = methodsym(method, t, 0)
		sig.type_ = methodfunc(f.Type, t)
		sig.mtype = methodfunc(f.Type, nil)

		if sig.isym.Flags&SymSiggen == 0 {
			sig.isym.Flags |= SymSiggen
			if !Eqtype(this, it) || this.Width < Types[Tptr].Width {
				compiling_wrappers = 1
				genwrapper(it, f, sig.isym, 1)
				compiling_wrappers = 0
			}
		}

		if sig.tsym.Flags&SymSiggen == 0 {
			sig.tsym.Flags |= SymSiggen
			if !Eqtype(this, t) {
				compiling_wrappers = 1
				genwrapper(t, f, sig.tsym, 0)
				compiling_wrappers = 0
			}
		}
	}

	sort.Sort(byMethodNameAndPackagePath(ms))
	return ms
}

// imethods returns the methods of the interface type t, sorted by name.
func imethods(t *Type) []*Sig {
	var methods []*Sig
	for _, f := range t.Fields().Slice() {
		if f.Type.Etype != TFUNC || f.Sym == nil {
			continue
		}
		method := f.Sym
		var sig = Sig{
			name: method.Name,
		}
		if !exportname(method.Name) {
			if method.Pkg == nil {
				Fatalf("imethods: missing package")
			}
			sig.pkg = method.Pkg
		}

		sig.mtype = f.Type
		sig.offset = 0
		sig.type_ = methodfunc(f.Type, nil)

		if n := len(methods); n > 0 {
			last := methods[n-1]
			if !(siglt(last, &sig)) {
				Fatalf("sigcmp vs sortinter %s %s", last.name, sig.name)
			}
		}
		methods = append(methods, &sig)

		// Compiler can only refer to wrappers for non-blank methods.
		if isblanksym(method) {
			continue
		}

		// NOTE(rsc): Perhaps an oversight that
		// IfaceType.Method is not in the reflect data.
		// Generate the method body, so that compiled
		// code can refer to it.
		isym := methodsym(method, t, 0)

		if isym.Flags&SymSiggen == 0 {
			isym.Flags |= SymSiggen
			genwrapper(t, f, isym, 0)
		}
	}

	return methods
}

var dimportpath_gopkg *Pkg

func dimportpath(p *Pkg) {
	if p.Pathsym != nil {
		return
	}

	// If we are compiling the runtime package, there are two runtime packages around
	// -- localpkg and Runtimepkg. We don't want to produce import path symbols for
	// both of them, so just produce one for localpkg.
	if myimportpath == "runtime" && p == Runtimepkg {
		return
	}

	if dimportpath_gopkg == nil {
		dimportpath_gopkg = mkpkg("go")
		dimportpath_gopkg.Name = "go"
	}

	nam := "importpath." + p.Prefix + "."

	n := Nod(ONAME, nil, nil)
	n.Sym = Pkglookup(nam, dimportpath_gopkg)

	n.Class = PEXTERN
	n.Xoffset = 0
	p.Pathsym = n.Sym

	if p == localpkg {
		// Note: myimportpath != "", or else dgopkgpath won't call dimportpath.
		gdatastring(n, myimportpath)
	} else {
		gdatastring(n, p.Path)
	}
	ggloblsym(n.Sym, int32(Types[TSTRING].Width), obj.DUPOK|obj.RODATA)
}

func dgopkgpath(s *Sym, ot int, pkg *Pkg) int {
	return dgopkgpathLSym(Linksym(s), ot, pkg)
}

func dgopkgpathLSym(s *obj.LSym, ot int, pkg *Pkg) int {
	if pkg == nil {
		return duintxxLSym(s, ot, 0, Widthptr)
	}

	if pkg == localpkg && myimportpath == "" {
		// If we don't know the full import path of the package being compiled
		// (i.e. -p was not passed on the compiler command line), emit a reference to
		// go.importpath.""., which the linker will rewrite using the correct import path.
		// Every package that imports this one directly defines the symbol.
		// See also https://groups.google.com/forum/#!topic/golang-dev/myb9s53HxGQ.
		ns := obj.Linklookup(Ctxt, `go.importpath."".`, 0)
		return dsymptrLSym(s, ot, ns, 0)
	}

	dimportpath(pkg)
	return dsymptrLSym(s, ot, Linksym(pkg.Pathsym), 0)
}

// isExportedField reports whether a struct field is exported.
func isExportedField(ft *Field) bool {
	if ft.Sym != nil && ft.Embedded == 0 {
		return exportname(ft.Sym.Name)
	} else {
		if ft.Type.Sym != nil &&
			(ft.Type.Sym.Pkg == builtinpkg || !exportname(ft.Type.Sym.Name)) {
			return false
		} else {
			return true
		}
	}
}

// dnameField dumps a reflect.name for a struct field.
func dnameField(s *Sym, ot int, ft *Field) int {
	var name, tag string
	if ft.Sym != nil && ft.Embedded == 0 {
		name = ft.Sym.Name
	}
	if ft.Note != nil {
		tag = *ft.Note
	}
	return dname(s, ot, name, tag, nil, isExportedField(ft))
}

var dnameCount int

// dname dumps a reflect.name for a struct field or method.
func dname(s *Sym, ot int, name, tag string, pkg *Pkg, exported bool) int {
	if len(name) > 1<<16-1 {
		Fatalf("name too long: %s", name)
	}
	if len(tag) > 1<<16-1 {
		Fatalf("tag too long: %s", tag)
	}

	// Encode name and tag. See reflect/type.go for details.
	var bits byte
	l := 1 + 2 + len(name)
	if exported {
		bits |= 1 << 0
	}
	if len(tag) > 0 {
		l += 2 + len(tag)
		bits |= 1 << 1
	}
	if pkg != nil {
		bits |= 1 << 2
	}
	b := make([]byte, l)
	b[0] = bits
	b[1] = uint8(len(name) >> 8)
	b[2] = uint8(len(name))
	copy(b[3:], name)
	if len(tag) > 0 {
		tb := b[3+len(name):]
		tb[0] = uint8(len(tag) >> 8)
		tb[1] = uint8(len(tag))
		copy(tb[2:], tag)
	}

	// Very few names require a pkgPath *string (only those
	// defined in a different package than their type). So if
	// there is no pkgPath, we treat the name contents as string
	// data that duplicates across packages.
	var bsym *obj.LSym
	if pkg == nil {
		_, bsym = stringsym(string(b))
	} else {
		bsymname := fmt.Sprintf(`go.string."".methodname.%d`, dnameCount)
		dnameCount++
		bsym = obj.Linklookup(Ctxt, bsymname, 0)
		bsym.P = b
		boff := len(b)
		boff = int(Rnd(int64(boff), int64(Widthptr)))
		boff = dgopkgpathLSym(bsym, boff, pkg)
		ggloblLSym(bsym, int32(boff), obj.RODATA|obj.LOCAL)
	}

	ot = dsymptrLSym(Linksym(s), ot, bsym, 0)

	return ot
}

// dextratype dumps the fields of a runtime.uncommontype.
// dataAdd is the offset in bytes after the header where the
// backing array of the []method field is written (by dextratypeData).
func dextratype(s *Sym, ot int, t *Type, dataAdd int) int {
	m := methods(t)
	if t.Sym == nil && len(m) == 0 {
		return ot
	}
	noff := int(Rnd(int64(ot), int64(Widthptr)))
	if noff != ot {
		Fatalf("unexpected alignment in dextratype for %s", t)
	}

	for _, a := range m {
		dtypesym(a.type_)
	}

	ot = dgopkgpath(s, ot, typePkg(t))

	// slice header
	ot = dsymptr(s, ot, s, ot+Widthptr+2*Widthint+dataAdd)

	n := len(m)
	ot = duintxx(s, ot, uint64(n), Widthint)
	ot = duintxx(s, ot, uint64(n), Widthint)

	return ot
}

func typePkg(t *Type) *Pkg {
	tsym := t.Sym
	if tsym == nil && t.Type != nil {
		tsym = t.Type.Sym
	}
	if tsym != nil && t != Types[t.Etype] && t != errortype {
		return tsym.Pkg
	}
	return nil
}

// dextratypeData dumps the backing array for the []method field of
// runtime.uncommontype.
func dextratypeData(s *Sym, ot int, t *Type) int {
	for _, a := range methods(t) {
		// ../../../../runtime/type.go:/method
		exported := exportname(a.name)
		var pkg *Pkg
		if !exported && a.pkg != typePkg(t) {
			pkg = a.pkg
		}
		ot = dname(s, ot, a.name, "", pkg, exported)
		ot = dmethodptr(s, ot, dtypesym(a.mtype))
		ot = dmethodptr(s, ot, a.isym)
		ot = dmethodptr(s, ot, a.tsym)
	}
	return ot
}

func dmethodptr(s *Sym, off int, x *Sym) int {
	duintptr(s, off, 0)
	r := obj.Addrel(Linksym(s))
	r.Off = int32(off)
	r.Siz = uint8(Widthptr)
	r.Sym = Linksym(x)
	r.Type = obj.R_METHOD
	return off + Widthptr
}

var kinds = []int{
	TINT:        obj.KindInt,
	TUINT:       obj.KindUint,
	TINT8:       obj.KindInt8,
	TUINT8:      obj.KindUint8,
	TINT16:      obj.KindInt16,
	TUINT16:     obj.KindUint16,
	TINT32:      obj.KindInt32,
	TUINT32:     obj.KindUint32,
	TINT64:      obj.KindInt64,
	TUINT64:     obj.KindUint64,
	TUINTPTR:    obj.KindUintptr,
	TFLOAT32:    obj.KindFloat32,
	TFLOAT64:    obj.KindFloat64,
	TBOOL:       obj.KindBool,
	TSTRING:     obj.KindString,
	TPTR32:      obj.KindPtr,
	TPTR64:      obj.KindPtr,
	TSTRUCT:     obj.KindStruct,
	TINTER:      obj.KindInterface,
	TCHAN:       obj.KindChan,
	TMAP:        obj.KindMap,
	TARRAY:      obj.KindArray,
	TFUNC:       obj.KindFunc,
	TCOMPLEX64:  obj.KindComplex64,
	TCOMPLEX128: obj.KindComplex128,
	TUNSAFEPTR:  obj.KindUnsafePointer,
}

func haspointers(t *Type) bool {
	if t.Haspointers != 0 {
		return t.Haspointers-1 != 0
	}

	var ret bool
	switch t.Etype {
	case TINT,
		TUINT,
		TINT8,
		TUINT8,
		TINT16,
		TUINT16,
		TINT32,
		TUINT32,
		TINT64,
		TUINT64,
		TUINTPTR,
		TFLOAT32,
		TFLOAT64,
		TCOMPLEX64,
		TCOMPLEX128,
		TBOOL:
		ret = false

	case TARRAY:
		if t.Bound < 0 { // slice
			ret = true
			break
		}

		if t.Bound == 0 { // empty array
			ret = false
			break
		}

		ret = haspointers(t.Type)

	case TSTRUCT:
		ret = false
		for _, t1 := range t.Fields().Slice() {
			if haspointers(t1.Type) {
				ret = true
				break
			}
		}

	case TSTRING,
		TPTR32,
		TPTR64,
		TUNSAFEPTR,
		TINTER,
		TCHAN,
		TMAP,
		TFUNC:
		fallthrough
	default:
		ret = true
	}

	t.Haspointers = 1 + uint8(obj.Bool2int(ret))
	return ret
}

// typeptrdata returns the length in bytes of the prefix of t
// containing pointer data. Anything after this offset is scalar data.
func typeptrdata(t *Type) int64 {
	if !haspointers(t) {
		return 0
	}

	switch t.Etype {
	case TPTR32,
		TPTR64,
		TUNSAFEPTR,
		TFUNC,
		TCHAN,
		TMAP:
		return int64(Widthptr)

	case TSTRING:
		// struct { byte *str; intgo len; }
		return int64(Widthptr)

	case TINTER:
		// struct { Itab *tab;	void *data; } or
		// struct { Type *type; void *data; }
		return 2 * int64(Widthptr)

	case TARRAY:
		if Isslice(t) {
			// struct { byte *array; uintgo len; uintgo cap; }
			return int64(Widthptr)
		}
		// haspointers already eliminated t.Bound == 0.
		return (t.Bound-1)*t.Type.Width + typeptrdata(t.Type)

	case TSTRUCT:
		// Find the last field that has pointers.
		var lastPtrField *Field
		for _, t1 := range t.Fields().Slice() {
			if haspointers(t1.Type) {
				lastPtrField = t1
			}
		}
		return lastPtrField.Width + typeptrdata(lastPtrField.Type)

	default:
		Fatalf("typeptrdata: unexpected type, %v", t)
		return 0
	}
}

// tflag is documented in ../../../../reflect/type.go.
const tflagUncommon = 1

// commonType
// ../../../../runtime/type.go:/commonType

var dcommontype_algarray *Sym

func dcommontype(s *Sym, ot int, t *Type) int {
	if ot != 0 {
		Fatalf("dcommontype %d", ot)
	}

	sizeofAlg := 2 * Widthptr
	if dcommontype_algarray == nil {
		dcommontype_algarray = Pkglookup("algarray", Runtimepkg)
	}
	dowidth(t)
	alg := algtype(t)
	var algsym *Sym
	if alg < 0 || alg == AMEM {
		algsym = dalgsym(t)
	}

	tptr := Ptrto(t)
	if !Isptr[t.Etype] && (t.Sym != nil || methods(tptr) != nil) {
		sptr := dtypesym(tptr)
		r := obj.Addrel(Linksym(s))
		r.Off = 0
		r.Siz = 0
		r.Sym = sptr.Lsym
		r.Type = obj.R_USETYPE
	}

	gcsym, useGCProg, ptrdata := dgcsym(t)

	// ../../../../reflect/type.go:/^type.rtype
	// actual type structure
	//	type rtype struct {
	//		size          uintptr
	//		ptrdata       uintptr
	//		hash          uint32
	//		tflag         tflag
	//		align         uint8
	//		fieldAlign    uint8
	//		kind          uint8
	//		alg           *typeAlg
	//		gcdata        *byte
	//		string        *string
	//	}
	ot = duintptr(s, ot, uint64(t.Width))
	ot = duintptr(s, ot, uint64(ptrdata))

	ot = duint32(s, ot, typehash(t))

	var tflag uint8
	if uncommonSize(t) != 0 {
		tflag |= tflagUncommon
	}
	ot = duint8(s, ot, tflag)

	// runtime (and common sense) expects alignment to be a power of two.
	i := int(t.Align)

	if i == 0 {
		i = 1
	}
	if i&(i-1) != 0 {
		Fatalf("invalid alignment %d for %v", t.Align, t)
	}
	ot = duint8(s, ot, t.Align) // align
	ot = duint8(s, ot, t.Align) // fieldAlign

	i = kinds[t.Etype]
	if t.Etype == TARRAY && t.Bound < 0 {
		i = obj.KindSlice
	}
	if !haspointers(t) {
		i |= obj.KindNoPointers
	}
	if isdirectiface(t) {
		i |= obj.KindDirectIface
	}
	if useGCProg {
		i |= obj.KindGCProg
	}
	ot = duint8(s, ot, uint8(i)) // kind
	if algsym == nil {
		ot = dsymptr(s, ot, dcommontype_algarray, alg*sizeofAlg)
	} else {
		ot = dsymptr(s, ot, algsym, 0)
	}
	ot = dsymptr(s, ot, gcsym, 0) // gcdata

	p := Tconv(t, FmtLeft|FmtUnsigned)

	// If we're writing out type T,
	// we are very likely to write out type *T as well.
	// Use the string "*T"[1:] for "T", so that the two
	// share storage. This is a cheap way to reduce the
	// amount of space taken up by reflect strings.
	prefix := 0
	if !strings.HasPrefix(p, "*") {
		p = "*" + p
		prefix = 1
	}
	_, symdata := stringsym(p) // string
	ot = dsymptrLSym(Linksym(s), ot, symdata, prefix)
	ot = duintxx(s, ot, uint64(len(p)-prefix), Widthint)

	return ot
}

func typesym(t *Type) *Sym {
	return Pkglookup(Tconv(t, FmtLeft), typepkg)
}

// tracksym returns the symbol for tracking use of field/method f, assumed
// to be a member of struct/interface type t.
func tracksym(t *Type, f *Field) *Sym {
	return Pkglookup(Tconv(t, FmtLeft)+"."+f.Sym.Name, trackpkg)
}

func typelinksym(t *Type) *Sym {
	// %-uT is what the generated Type's string field says.
	// It uses (ambiguous) package names instead of import paths.
	// %-T is the complete, unambiguous type name.
	// We want the types to end up sorted by string field,
	// so use that first in the name, and then add :%-T to
	// disambiguate. We use a tab character as the separator to
	// ensure the types appear sorted by their string field. The
	// names are a little long but they are discarded by the linker
	// and do not end up in the symbol table of the final binary.
	p := Tconv(t, FmtLeft|FmtUnsigned) + "\t" + Tconv(t, FmtLeft)

	s := Pkglookup(p, typelinkpkg)

	//print("typelinksym: %s -> %+S\n", p, s);

	return s
}

func typesymprefix(prefix string, t *Type) *Sym {
	p := prefix + "." + Tconv(t, FmtLeft)
	s := Pkglookup(p, typepkg)

	//print("algsym: %s -> %+S\n", p, s);

	return s
}

func typenamesym(t *Type) *Sym {
	if t == nil || (Isptr[t.Etype] && t.Type == nil) || isideal(t) {
		Fatalf("typename %v", t)
	}
	s := typesym(t)
	if s.Def == nil {
		n := Nod(ONAME, nil, nil)
		n.Sym = s
		n.Type = Types[TUINT8]
		n.Addable = true
		n.Ullman = 1
		n.Class = PEXTERN
		n.Xoffset = 0
		n.Typecheck = 1
		s.Def = n

		signatlist = append(signatlist, typenod(t))
	}

	return s.Def.Sym
}

func typename(t *Type) *Node {
	s := typenamesym(t)
	n := Nod(OADDR, s.Def, nil)
	n.Type = Ptrto(s.Def.Type)
	n.Addable = true
	n.Ullman = 2
	n.Typecheck = 1
	return n
}

// isreflexive reports whether t has a reflexive equality operator.
// That is, if x==x for all x of type t.
func isreflexive(t *Type) bool {
	switch t.Etype {
	case TBOOL,
		TINT,
		TUINT,
		TINT8,
		TUINT8,
		TINT16,
		TUINT16,
		TINT32,
		TUINT32,
		TINT64,
		TUINT64,
		TUINTPTR,
		TPTR32,
		TPTR64,
		TUNSAFEPTR,
		TSTRING,
		TCHAN:
		return true

	case TFLOAT32,
		TFLOAT64,
		TCOMPLEX64,
		TCOMPLEX128,
		TINTER:
		return false

	case TARRAY:
		if Isslice(t) {
			Fatalf("slice can't be a map key: %v", t)
		}
		return isreflexive(t.Type)

	case TSTRUCT:
		for _, t1 := range t.Fields().Slice() {
			if !isreflexive(t1.Type) {
				return false
			}
		}
		return true

	default:
		Fatalf("bad type for map key: %v", t)
		return false
	}
}

// needkeyupdate reports whether map updates with t as a key
// need the key to be updated.
func needkeyupdate(t *Type) bool {
	switch t.Etype {
	case TBOOL,
		TINT,
		TUINT,
		TINT8,
		TUINT8,
		TINT16,
		TUINT16,
		TINT32,
		TUINT32,
		TINT64,
		TUINT64,
		TUINTPTR,
		TPTR32,
		TPTR64,
		TUNSAFEPTR,
		TCHAN:
		return false

	case TFLOAT32, // floats can be +0/-0
		TFLOAT64,
		TCOMPLEX64,
		TCOMPLEX128,
		TINTER,
		TSTRING: // strings might have smaller backing stores
		return true

	case TARRAY:
		if Isslice(t) {
			Fatalf("slice can't be a map key: %v", t)
		}
		return needkeyupdate(t.Type)

	case TSTRUCT:
		for _, t1 := range t.Fields().Slice() {
			if needkeyupdate(t1.Type) {
				return true
			}
		}
		return false

	default:
		Fatalf("bad type for map key: %v", t)
		return true
	}
}

func dtypesym(t *Type) *Sym {
	// Replace byte, rune aliases with real type.
	// They've been separate internally to make error messages
	// better, but we have to merge them in the reflect tables.
	if t == bytetype || t == runetype {
		t = Types[t.Etype]
	}

	if isideal(t) {
		Fatalf("dtypesym %v", t)
	}

	s := typesym(t)
	if s.Flags&SymSiggen != 0 {
		return s
	}
	s.Flags |= SymSiggen

	// special case (look for runtime below):
	// when compiling package runtime,
	// emit the type structures for int, float, etc.
	tbase := t

	if Isptr[t.Etype] && t.Sym == nil && t.Type.Sym != nil {
		tbase = t.Type
	}
	dupok := 0
	if tbase.Sym == nil {
		dupok = obj.DUPOK
	}

	if myimportpath == "runtime" && (tbase == Types[tbase.Etype] || tbase == bytetype || tbase == runetype || tbase == errortype) { // int, float, etc
		goto ok
	}

	// named types from other files are defined only by those files
	if tbase.Sym != nil && !tbase.Local {
		return s
	}
	if isforw[tbase.Etype] {
		return s
	}

ok:
	ot := 0
	switch t.Etype {
	default:
		ot = dcommontype(s, ot, t)
		ot = dextratype(s, ot, t, 0)

	case TARRAY:
		if t.Bound >= 0 {
			// ../../../../runtime/type.go:/arrayType
			s1 := dtypesym(t.Type)

			t2 := typ(TARRAY)
			t2.Type = t.Type
			t2.Bound = -1 // slice
			s2 := dtypesym(t2)
			ot = dcommontype(s, ot, t)
			ot = dsymptr(s, ot, s1, 0)
			ot = dsymptr(s, ot, s2, 0)
			ot = duintptr(s, ot, uint64(t.Bound))
		} else {
			// ../../../../runtime/type.go:/sliceType
			s1 := dtypesym(t.Type)

			ot = dcommontype(s, ot, t)
			ot = dsymptr(s, ot, s1, 0)
		}
		ot = dextratype(s, ot, t, 0)

	// ../../../../runtime/type.go:/chanType
	case TCHAN:
		s1 := dtypesym(t.Type)

		ot = dcommontype(s, ot, t)
		ot = dsymptr(s, ot, s1, 0)
		ot = duintptr(s, ot, uint64(t.Chan))
		ot = dextratype(s, ot, t, 0)

	case TFUNC:
		for _, t1 := range t.Recvs().Fields().Slice() {
			dtypesym(t1.Type)
		}
		isddd := false
		for _, t1 := range t.Params().Fields().Slice() {
			isddd = t1.Isddd
			dtypesym(t1.Type)
		}
		for _, t1 := range t.Results().Fields().Slice() {
			dtypesym(t1.Type)
		}

		ot = dcommontype(s, ot, t)
		inCount := t.Recvs().NumFields() + t.Params().NumFields()
		outCount := t.Results().NumFields()
		if isddd {
			outCount |= 1 << 15
		}
		ot = duint16(s, ot, uint16(inCount))
		ot = duint16(s, ot, uint16(outCount))
		if Widthptr == 8 {
			ot += 4 // align for *rtype
		}

		dataAdd := (inCount + t.Results().NumFields()) * Widthptr
		ot = dextratype(s, ot, t, dataAdd)

		// Array of rtype pointers follows funcType.
		for _, t1 := range t.Recvs().Fields().Slice() {
			ot = dsymptr(s, ot, dtypesym(t1.Type), 0)
		}
		for _, t1 := range t.Params().Fields().Slice() {
			ot = dsymptr(s, ot, dtypesym(t1.Type), 0)
		}
		for _, t1 := range t.Results().Fields().Slice() {
			ot = dsymptr(s, ot, dtypesym(t1.Type), 0)
		}

	case TINTER:
		m := imethods(t)
		n := len(m)
		for _, a := range m {
			dtypesym(a.type_)
		}

		// ../../../../runtime/type.go:/interfaceType
		ot = dcommontype(s, ot, t)

		var tpkg *Pkg
		if t.Sym != nil && t != Types[t.Etype] && t != errortype {
			tpkg = t.Sym.Pkg
		}
		ot = dgopkgpath(s, ot, tpkg)

		ot = dsymptr(s, ot, s, ot+Widthptr+2*Widthint+uncommonSize(t))
		ot = duintxx(s, ot, uint64(n), Widthint)
		ot = duintxx(s, ot, uint64(n), Widthint)
		dataAdd := imethodSize() * n
		ot = dextratype(s, ot, t, dataAdd)

		for _, a := range m {
			// ../../../../runtime/type.go:/imethod
			exported := exportname(a.name)
			var pkg *Pkg
			if !exported && a.pkg != tpkg {
				pkg = a.pkg
			}
			ot = dname(s, ot, a.name, "", pkg, exported)
			ot = dsymptr(s, ot, dtypesym(a.type_), 0)
		}

	// ../../../../runtime/type.go:/mapType
	case TMAP:
		s1 := dtypesym(t.Key())
		s2 := dtypesym(t.Type)
		s3 := dtypesym(mapbucket(t))
		s4 := dtypesym(hmap(t))
		ot = dcommontype(s, ot, t)
		ot = dsymptr(s, ot, s1, 0)
		ot = dsymptr(s, ot, s2, 0)
		ot = dsymptr(s, ot, s3, 0)
		ot = dsymptr(s, ot, s4, 0)
		if t.Key().Width > MAXKEYSIZE {
			ot = duint8(s, ot, uint8(Widthptr))
			ot = duint8(s, ot, 1) // indirect
		} else {
			ot = duint8(s, ot, uint8(t.Key().Width))
			ot = duint8(s, ot, 0) // not indirect
		}

		if t.Type.Width > MAXVALSIZE {
			ot = duint8(s, ot, uint8(Widthptr))
			ot = duint8(s, ot, 1) // indirect
		} else {
			ot = duint8(s, ot, uint8(t.Type.Width))
			ot = duint8(s, ot, 0) // not indirect
		}

		ot = duint16(s, ot, uint16(mapbucket(t).Width))
		ot = duint8(s, ot, uint8(obj.Bool2int(isreflexive(t.Key()))))
		ot = duint8(s, ot, uint8(obj.Bool2int(needkeyupdate(t.Key()))))
		ot = dextratype(s, ot, t, 0)

	case TPTR32, TPTR64:
		if t.Type.Etype == TANY {
			// ../../../../runtime/type.go:/UnsafePointerType
			ot = dcommontype(s, ot, t)
			ot = dextratype(s, ot, t, 0)

			break
		}

		// ../../../../runtime/type.go:/ptrType
		s1 := dtypesym(t.Type)

		ot = dcommontype(s, ot, t)
		ot = dsymptr(s, ot, s1, 0)
		ot = dextratype(s, ot, t, 0)

	// ../../../../runtime/type.go:/structType
	// for security, only the exported fields.
	case TSTRUCT:
		n := 0

		for _, t1 := range t.Fields().Slice() {
			dtypesym(t1.Type)
			n++
		}

		ot = dcommontype(s, ot, t)
		var pkg *Pkg
		if t.Sym != nil {
			pkg = t.Sym.Pkg
		}
		ot = dgopkgpath(s, ot, pkg)
		ot = dsymptr(s, ot, s, ot+Widthptr+2*Widthint+uncommonSize(t))
		ot = duintxx(s, ot, uint64(n), Widthint)
		ot = duintxx(s, ot, uint64(n), Widthint)

		dataAdd := n * structfieldSize()
		ot = dextratype(s, ot, t, dataAdd)

		for _, f := range t.Fields().Slice() {
			// ../../../../runtime/type.go:/structField
			ot = dnameField(s, ot, f)
			ot = dsymptr(s, ot, dtypesym(f.Type), 0)
			ot = duintptr(s, ot, uint64(f.Width)) // field offset
		}
	}

	ot = dextratypeData(s, ot, t)
	ggloblsym(s, int32(ot), int16(dupok|obj.RODATA))

	// generate typelink.foo pointing at s = type.foo.
	// The linker will leave a table of all the typelinks for
	// types in the binary, so reflect can find them.
	// We only need the link for unnamed composites that
	// we want be able to find.
	if t.Sym == nil {
		switch t.Etype {
		case TPTR32, TPTR64, TARRAY, TCHAN, TFUNC, TMAP:
			slink := typelinksym(t)
			dsymptr(slink, 0, s, 0)
			ggloblsym(slink, int32(Widthptr), int16(dupok|obj.RODATA))
		}
	}

	return s
}

func dumptypestructs() {
	// copy types from externdcl list to signatlist
	for _, n := range externdcl {
		if n.Op != OTYPE {
			continue
		}
		signatlist = append(signatlist, n)
	}

	// Process signatlist.  This can't use range, as entries are
	// added to the list while it is being processed.
	for i := 0; i < len(signatlist); i++ {
		n := signatlist[i]
		if n.Op != OTYPE {
			continue
		}
		t := n.Type
		dtypesym(t)
		if t.Sym != nil {
			dtypesym(Ptrto(t))
		}
	}

	// generate import strings for imported packages
	for _, p := range pkgs {
		if p.Direct {
			dimportpath(p)
		}
	}

	// do basic types if compiling package runtime.
	// they have to be in at least one package,
	// and runtime is always loaded implicitly,
	// so this is as good as any.
	// another possible choice would be package main,
	// but using runtime means fewer copies in .6 files.
	if myimportpath == "runtime" {
		for i := EType(1); i <= TBOOL; i++ {
			dtypesym(Ptrto(Types[i]))
		}
		dtypesym(Ptrto(Types[TSTRING]))
		dtypesym(Ptrto(Types[TUNSAFEPTR]))

		// emit type structs for error and func(error) string.
		// The latter is the type of an auto-generated wrapper.
		dtypesym(Ptrto(errortype))

		dtypesym(functype(nil, []*Node{Nod(ODCLFIELD, nil, typenod(errortype))}, []*Node{Nod(ODCLFIELD, nil, typenod(Types[TSTRING]))}))

		// add paths for runtime and main, which 6l imports implicitly.
		dimportpath(Runtimepkg)

		if flag_race != 0 {
			dimportpath(racepkg)
		}
		if flag_msan != 0 {
			dimportpath(msanpkg)
		}
		dimportpath(mkpkg("main"))
	}
}

func dalgsym(t *Type) *Sym {
	var s *Sym
	var hashfunc *Sym
	var eqfunc *Sym

	// dalgsym is only called for a type that needs an algorithm table,
	// which implies that the type is comparable (or else it would use ANOEQ).

	if algtype(t) == AMEM {
		// we use one algorithm table for all AMEM types of a given size
		p := fmt.Sprintf(".alg%d", t.Width)

		s = Pkglookup(p, typepkg)

		if s.Flags&SymAlgGen != 0 {
			return s
		}
		s.Flags |= SymAlgGen

		// make hash closure
		p = fmt.Sprintf(".hashfunc%d", t.Width)

		hashfunc = Pkglookup(p, typepkg)

		ot := 0
		ot = dsymptr(hashfunc, ot, Pkglookup("memhash_varlen", Runtimepkg), 0)
		ot = duintxx(hashfunc, ot, uint64(t.Width), Widthptr) // size encoded in closure
		ggloblsym(hashfunc, int32(ot), obj.DUPOK|obj.RODATA)

		// make equality closure
		p = fmt.Sprintf(".eqfunc%d", t.Width)

		eqfunc = Pkglookup(p, typepkg)

		ot = 0
		ot = dsymptr(eqfunc, ot, Pkglookup("memequal_varlen", Runtimepkg), 0)
		ot = duintxx(eqfunc, ot, uint64(t.Width), Widthptr)
		ggloblsym(eqfunc, int32(ot), obj.DUPOK|obj.RODATA)
	} else {
		// generate an alg table specific to this type
		s = typesymprefix(".alg", t)

		hash := typesymprefix(".hash", t)
		eq := typesymprefix(".eq", t)
		hashfunc = typesymprefix(".hashfunc", t)
		eqfunc = typesymprefix(".eqfunc", t)

		genhash(hash, t)
		geneq(eq, t)

		// make Go funcs (closures) for calling hash and equal from Go
		dsymptr(hashfunc, 0, hash, 0)

		ggloblsym(hashfunc, int32(Widthptr), obj.DUPOK|obj.RODATA)
		dsymptr(eqfunc, 0, eq, 0)
		ggloblsym(eqfunc, int32(Widthptr), obj.DUPOK|obj.RODATA)
	}

	// ../../../../runtime/alg.go:/typeAlg
	ot := 0

	ot = dsymptr(s, ot, hashfunc, 0)
	ot = dsymptr(s, ot, eqfunc, 0)
	ggloblsym(s, int32(ot), obj.DUPOK|obj.RODATA)
	return s
}

// maxPtrmaskBytes is the maximum length of a GC ptrmask bitmap,
// which holds 1-bit entries describing where pointers are in a given type.
// 16 bytes is enough to describe 128 pointer-sized words, 512 or 1024 bytes
// depending on the system. Above this length, the GC information is
// recorded as a GC program, which can express repetition compactly.
// In either form, the information is used by the runtime to initialize the
// heap bitmap, and for large types (like 128 or more words), they are
// roughly the same speed. GC programs are never much larger and often
// more compact. (If large arrays are involved, they can be arbitrarily more
// compact.)
//
// The cutoff must be large enough that any allocation large enough to
// use a GC program is large enough that it does not share heap bitmap
// bytes with any other objects, allowing the GC program execution to
// assume an aligned start and not use atomic operations. In the current
// runtime, this means all malloc size classes larger than the cutoff must
// be multiples of four words. On 32-bit systems that's 16 bytes, and
// all size classes >= 16 bytes are 16-byte aligned, so no real constraint.
// On 64-bit systems, that's 32 bytes, and 32-byte alignment is guaranteed
// for size classes >= 256 bytes. On a 64-bit system, 256 bytes allocated
// is 32 pointers, the bits for which fit in 4 bytes. So maxPtrmaskBytes
// must be >= 4.
//
// We used to use 16 because the GC programs do have some constant overhead
// to get started, and processing 128 pointers seems to be enough to
// amortize that overhead well.
//
// To make sure that the runtime's chansend can call typeBitsBulkBarrier,
// we raised the limit to 2048, so that even 32-bit systems are guaranteed to
// use bitmaps for objects up to 64 kB in size.
//
// Also known to reflect/type.go.
//
const maxPtrmaskBytes = 2048

// dgcsym emits and returns a data symbol containing GC information for type t,
// along with a boolean reporting whether the UseGCProg bit should be set in
// the type kind, and the ptrdata field to record in the reflect type information.
func dgcsym(t *Type) (sym *Sym, useGCProg bool, ptrdata int64) {
	ptrdata = typeptrdata(t)
	if ptrdata/int64(Widthptr) <= maxPtrmaskBytes*8 {
		sym = dgcptrmask(t)
		return
	}

	useGCProg = true
	sym, ptrdata = dgcprog(t)
	return
}

// dgcptrmask emits and returns the symbol containing a pointer mask for type t.
func dgcptrmask(t *Type) *Sym {
	ptrmask := make([]byte, (typeptrdata(t)/int64(Widthptr)+7)/8)
	fillptrmask(t, ptrmask)
	p := fmt.Sprintf("gcbits.%x", ptrmask)

	sym := Pkglookup(p, Runtimepkg)
	if sym.Flags&SymUniq == 0 {
		sym.Flags |= SymUniq
		for i, x := range ptrmask {
			duint8(sym, i, x)
		}
		ggloblsym(sym, int32(len(ptrmask)), obj.DUPOK|obj.RODATA|obj.LOCAL)
	}
	return sym
}

// fillptrmask fills in ptrmask with 1s corresponding to the
// word offsets in t that hold pointers.
// ptrmask is assumed to fit at least typeptrdata(t)/Widthptr bits.
func fillptrmask(t *Type, ptrmask []byte) {
	for i := range ptrmask {
		ptrmask[i] = 0
	}
	if !haspointers(t) {
		return
	}

	vec := bvalloc(8 * int32(len(ptrmask)))
	xoffset := int64(0)
	onebitwalktype1(t, &xoffset, vec)

	nptr := typeptrdata(t) / int64(Widthptr)
	for i := int64(0); i < nptr; i++ {
		if bvget(vec, int32(i)) == 1 {
			ptrmask[i/8] |= 1 << (uint(i) % 8)
		}
	}
}

// dgcprog emits and returns the symbol containing a GC program for type t
// along with the size of the data described by the program (in the range [typeptrdata(t), t.Width]).
// In practice, the size is typeptrdata(t) except for non-trivial arrays.
// For non-trivial arrays, the program describes the full t.Width size.
func dgcprog(t *Type) (*Sym, int64) {
	dowidth(t)
	if t.Width == BADWIDTH {
		Fatalf("dgcprog: %v badwidth", t)
	}
	sym := typesymprefix(".gcprog", t)
	var p GCProg
	p.init(sym)
	p.emit(t, 0)
	offset := p.w.BitIndex() * int64(Widthptr)
	p.end()
	if ptrdata := typeptrdata(t); offset < ptrdata || offset > t.Width {
		Fatalf("dgcprog: %v: offset=%d but ptrdata=%d size=%d", t, offset, ptrdata, t.Width)
	}
	return sym, offset
}

type GCProg struct {
	sym    *Sym
	symoff int
	w      gcprog.Writer
}

var Debug_gcprog int // set by -d gcprog

func (p *GCProg) init(sym *Sym) {
	p.sym = sym
	p.symoff = 4 // first 4 bytes hold program length
	p.w.Init(p.writeByte)
	if Debug_gcprog > 0 {
		fmt.Fprintf(os.Stderr, "compile: start GCProg for %v\n", sym)
		p.w.Debug(os.Stderr)
	}
}

func (p *GCProg) writeByte(x byte) {
	p.symoff = duint8(p.sym, p.symoff, x)
}

func (p *GCProg) end() {
	p.w.End()
	duint32(p.sym, 0, uint32(p.symoff-4))
	ggloblsym(p.sym, int32(p.symoff), obj.DUPOK|obj.RODATA|obj.LOCAL)
	if Debug_gcprog > 0 {
		fmt.Fprintf(os.Stderr, "compile: end GCProg for %v\n", p.sym)
	}
}

func (p *GCProg) emit(t *Type, offset int64) {
	dowidth(t)
	if !haspointers(t) {
		return
	}
	if t.Width == int64(Widthptr) {
		p.w.Ptr(offset / int64(Widthptr))
		return
	}
	switch t.Etype {
	default:
		Fatalf("GCProg.emit: unexpected type %v", t)

	case TSTRING:
		p.w.Ptr(offset / int64(Widthptr))

	case TINTER:
		p.w.Ptr(offset / int64(Widthptr))
		p.w.Ptr(offset/int64(Widthptr) + 1)

	case TARRAY:
		if Isslice(t) {
			p.w.Ptr(offset / int64(Widthptr))
			return
		}
		if t.Bound == 0 {
			// should have been handled by haspointers check above
			Fatalf("GCProg.emit: empty array")
		}

		// Flatten array-of-array-of-array to just a big array by multiplying counts.
		count := t.Bound
		elem := t.Type
		for Isfixedarray(elem) {
			count *= elem.Bound
			elem = elem.Type
		}

		if !p.w.ShouldRepeat(elem.Width/int64(Widthptr), count) {
			// Cheaper to just emit the bits.
			for i := int64(0); i < count; i++ {
				p.emit(elem, offset+i*elem.Width)
			}
			return
		}
		p.emit(elem, offset)
		p.w.ZeroUntil((offset + elem.Width) / int64(Widthptr))
		p.w.Repeat(elem.Width/int64(Widthptr), count-1)

	case TSTRUCT:
		for _, t1 := range t.Fields().Slice() {
			p.emit(t1.Type, offset+t1.Width)
		}
	}
}
