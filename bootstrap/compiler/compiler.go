package compiler

import (
	"bootstrap/lexer"
	"bootstrap/parser"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func containsPath(path string, imported []string) bool {
	for _, imp := range imported {
		if path == imp {
			return true
		}
	}
	return false
}

func getAll(ast parser.Ast, imported []string) ([]string, []*parser.Fun, []*parser.Let, []*parser.Type) {
	funs := ast.Funs
	lets := ast.Lets
	types := ast.Types
	for _, imp := range ast.Imports {
		currPath, err := filepath.Abs(".")
		if err != nil {
			panic(fmt.Sprintf("invalid import path '%s'", imp.Path.Content))
		}
		path, err := filepath.Abs(imp.Path.Content)
		if err != nil {
			panic(fmt.Sprintf("invalid import path '%s'", imp.Path.Content))
		}
		base := filepath.Dir(path)
		if os.Chdir(base) != nil {
			fmt.Println(path, base)
			panic(fmt.Sprintf("invalid import path '%s'", imp.Path.Content))
		}
		if !containsPath(path, imported) {
			imported = append(imported, path)
			dat, err := os.ReadFile(path)
			if err != nil {
				panic(fmt.Sprintf("invalid import path '%s'", imp.Path.Content))
			}
			newAst, perr := parser.Parse(lexer.New(string(dat)))
			if perr {
				panic(fmt.Sprintf("error parsing file '%s'", imp.Path.Content))
			}
			newImported, newFuns, newLets, newTypes := getAll(newAst, imported)
			imported = newImported
			funs = append(funs, newFuns...)
			lets = append(lets, newLets...)
			types = append(types, newTypes...)
		}
		if os.Chdir(currPath) != nil {
			panic(fmt.Sprintf("invalid import path '%s'", imp.Path.Content))
		}
	}
	return imported, funs, lets, types
}

func Compile(ast parser.Ast) []uint8 {
	_, allFuns, allLets, allTypes := getAll(ast, []string{})
	c := Ctx{}
	c.types = parser.NewTypes()
	for i := 0; i < len(allTypes); i++ {
		typ := allTypes[i]
		ident := typ.Ident.Content
		if c.types.Set(ident, typ) {
			panic(fmt.Sprintf("the type '%s' already exists", ident))
		}
	}

	c.lets = make(map[string]*Let)
	for i := 0; i < len(allLets); i++ {
		let := allLets[i]
		ident := let.Ident.Content
		if c.lets[ident] != nil {
			panic(fmt.Sprintf("the let '%s' already exists", ident))
		}
		c.lets[ident] = &Let{let: let}
	}

	c.funs = make(map[string]*Fun)
	for i := 0; i < len(allFuns); i++ {
		fun := allFuns[i]
		ident := c.makeFunIdent(fun.Ident.Content, fun.Inputs, fun.Outputs)
		if c.funs[ident] != nil {
			panic(fmt.Sprintf("the fun '%s' already exists", ident))
		}
		c.funs[ident] = &Fun{fun: fun}
	}
	return c.compile()
}

func (f *Fun) makeFunIdent(c *Ctx) string {
	return c.makeFunIdent(f.fun.Ident.Content, f.fun.Inputs, f.fun.Outputs)
}

func (c *Ctx) makeFunIdent(ident string, inputs []parser.Typ, outputs []parser.Typ) string {
	var buffer bytes.Buffer
	buffer.WriteString(ident)
	buffer.WriteString("(")
	for i := 0; i < len(inputs); i++ {
		if i != 0 {
			buffer.WriteString(",")
		}
		buffer.WriteString(inputs[i].String(c.types))
	}
	buffer.WriteString(":")
	for i := 0; i < len(outputs); i++ {
		if i != 0 {
			buffer.WriteString(",")
		}
		buffer.WriteString(outputs[i].String(c.types))
	}
	buffer.WriteString(")")
	return buffer.String()
}

type FInfo struct {
	inline          bool
	asm             bool
	unsafe          bool
	safe            bool
	simpleTypeCheck bool
	letSize         uint64
	size            uint64
	pos             uint64
	lets            map[string]*Let
}

type Fun struct {
	info *FInfo
	fun  *parser.Fun
}

func (f *Fun) getInfo(c *Ctx) *FInfo {
	if f.info == nil {
		f.comInfo(c)
	}
	return f.info
}

func (f *Fun) comInfo(c *Ctx) {
	if f.info != nil {
		return
	}

	f.info = &FInfo{}

	for _, opt := range f.fun.Opts {
		switch opt.Content {
		case "asm":
			f.info.asm = true
		case "safe":
			f.info.safe = true
		case "unsafe":
			f.info.unsafe = true
		case "inline":
			f.info.inline = true
		case "stc":
			f.info.simpleTypeCheck = true
		default:
			panic(fmt.Sprintf("unknown fun option '%s' for fun '%s'", opt.Content, f.makeFunIdent(c)))
		}
	}

	if f.info.asm && !f.info.inline {
		panic(fmt.Sprintf("the asm fun '%s' needs to also be inline", f.makeFunIdent(c)))
	}
	if f.info.asm && !(f.info.unsafe || f.info.safe) {
		panic(fmt.Sprintf("the asm fun '%s' needs to either be unsafe or allow unsafe", f.makeFunIdent(c)))
	}
	if f.info.simpleTypeCheck && !(f.info.unsafe || f.info.safe) {
		panic(fmt.Sprintf("the simple type check fun '%s' needs to either be unsafe or allow unsafe", f.makeFunIdent(c)))
	}

	if len(f.fun.Block.Lets) != 0 {
		if f.info.inline {
			panic(fmt.Sprintf("the inline fun '%s' can't have lets", f.makeFunIdent(c)))
		}
		if f.info.asm {
			panic(fmt.Sprintf("the asm fun '%s' can't have lets", f.makeFunIdent(c)))
		}

		f.info.lets = make(map[string]*Let)
		for _, let := range f.fun.Block.Lets {
			ident := let.Ident.Content
			if f.info.lets[ident] != nil {
				panic(fmt.Sprintf("the let '%s' already exists", ident))
			}
			f.info.lets[ident] = &Let{let: let}
			let := f.info.lets[ident]
			let.getInfo(c).pos = f.info.letSize
			f.info.letSize += let.info.size
			f.info.size += f.sizeOfExprs(c, let.let.Exprs) + let.info.loadSize
		}

		f.info.size += f.info.letSize
	}

	if f.info.asm {
		f.info.size += uint64(len(f.fun.Block.Exprs))
	} else {
		f.info.size += f.sizeOfExprs(c, f.fun.Block.Exprs)
	}

	if !f.info.inline {
		f.info.size += 1
		f.info.pos = c.getNextPos(f.info.size)
	}
}

func (f *Fun) sizeOfExprs(c *Ctx, exprs []parser.Expr) uint64 {
	var size uint64 = 0
	for i := 0; i < len(exprs); i++ {
		expr := exprs[i]
		ident := expr.AsIdent()
		call := expr.AsCall()
		number := expr.AsNumber()
		str := expr.AsString()
		ifel := expr.AsIf()
		while := expr.AsWhile()
		unwrap := expr.AsUnwrap()
		wrap := expr.AsWrap()
		addr := expr.AsAddr()
		ret := expr.AsReturn()
		if ident != nil {
			ident := ident.Content
			let := f.info.lets[ident]
			if let == nil {
				let = c.lets[ident]
				if let == nil {
					panic(fmt.Sprintf("unknown ident '%s'", ident))
				}
				let.getInfo(c)
			}
			size += let.info.loadSize
		} else if call != nil {
			ident := c.makeFunIdent(call.Ident.Content, call.Inputs, call.Outputs)
			fun := c.funs[ident]
			if fun == nil {
				panic(fmt.Sprintf("unknown fun '%s'", ident))
			}
			if fun.makeFunIdent(c) == c.start {
				panic(fmt.Sprintf("fun '%s' can't call '%s'", f.makeFunIdent(c), c.start))
			}
			finfo := fun.getInfo(c)
			if finfo.unsafe && !(f.info.unsafe || f.info.safe) {
				panic(fmt.Sprintf("fun '%s' can't call unsafe fun '%s'", f.makeFunIdent(c), ident))
			}
			if finfo.inline {
				size += finfo.size
			} else {
				size += 1 + 8
			}
		} else if number != nil {
			size += 1 + uint64(number.Size)
		} else if str != nil {
			c.pushStr(str.Content)
			size += 1 + 8 + 1 + 8
		} else if ifel != nil {
			size += f.sizeOfExprs(c, ifel.Con) + 1 + 8
			size += f.sizeOfExprs(c, ifel.Else) + 1 + 8
			size += f.sizeOfExprs(c, ifel.Exprs)
		} else if while != nil {
			size += f.sizeOfExprs(c, while.Con) + 1 + 8
			size += 1 + 8
			size += f.sizeOfExprs(c, while.Exprs) + 1 + 8
		} else if unwrap != nil {
			if !(f.info.unsafe || f.info.safe) {
				panic(fmt.Sprintf("fun '%s' can't call unsafe .unwrap", f.makeFunIdent(c)))
			}
		} else if wrap != nil {
			if !(f.info.unsafe || f.info.safe) {
				panic(fmt.Sprintf("fun '%s' can't call unsafe .wrap", f.makeFunIdent(c)))
			}
		} else if addr != nil {
			if !(f.info.unsafe || f.info.safe) {
				panic(fmt.Sprintf("fun '%s' can't call unsafe .addr", f.makeFunIdent(c)))
			}
			if addr.Ident != nil {
				ident := addr.Ident.Content
				if f.info.lets[ident] == nil {
					let := c.lets[ident]
					if let == nil {
						panic(fmt.Sprintf("unknown ident '%s'", ident))
					}
					let.getInfo(c)
				}
			} else {
				call := addr.Call
				ident := c.makeFunIdent(call.Ident.Content, call.Inputs, call.Outputs)
				fun := c.funs[ident]
				if fun == nil {
					panic(fmt.Sprintf("unknown fun '%s' in '%s'", ident, f.makeFunIdent(c)))
				}
				if fun.getInfo(c).inline {
					panic(fmt.Sprintf("can't get .addr of inline fun '%s' in '%s'", ident, f.makeFunIdent(c)))
				}
			}
			size += 1 + 8
		} else if ret != nil {
			if f.info.inline {
				panic(fmt.Sprintf("can't .return in inline fun '%s'", f.makeFunIdent(c)))
			}
			size += 1
		} else {
			panic("unreachable")
		}
	}
	return size
}

type LInfo struct {
	size     uint64
	loadSize uint64
	pos      uint64
}

type Let struct {
	info *LInfo
	let  *parser.Let
}

func (l *Let) getInfo(c *Ctx) *LInfo {
	if l.info == nil {
		l.comInfo(c)
	}
	return l.info
}

func (l *Let) comInfo(c *Ctx) {
	if l.info != nil {
		return
	}

	l.info = &LInfo{}
	l.info.size = uint64(l.let.Typ.Size(c.types))
	l.info.loadSize = 0
	for range l.let.Typ.LoadSizes(c.types) {
		l.info.loadSize += 1 + 8
	}
}

type Type struct {
	typ *parser.Type
}

type Ctx struct {
	size  uint64
	strs  string
	lets  map[string]*Let
	funs  map[string]*Fun
	types *parser.Types
	start string
}

func initialBytes() ([]uint8, int) {
	return []uint8{220, 0, 0, 0, 0, 0, 0, 0, 0}, 1
}

func (c *Ctx) compile() []uint8 {
	bytes, saddr := initialBytes()
	c.size = uint64(len(bytes))

	c.start = c.makeFunIdent(".start", []parser.Typ{parser.STRING}, []parser.Typ{})
	start := c.funs[c.start]

	if start == nil {
		panic(fmt.Sprintln("missing .start(string:) fun"))
	}

	sinfo := start.getInfo(c)
	if sinfo.inline {
		panic(fmt.Sprintln(".start(string:) can't be an inline fun"))
	}
	if !sinfo.unsafe {
		panic(fmt.Sprintln(".start(string:) needs to be unsafe"))
	}

	funs := []*Fun{}
	for _, fun := range c.funs {
		funs = append(funs, fun)
	}

	sort.Slice(funs, func(i, j int) bool {
		if funs[i].info == nil || funs[j].info == nil {
			return funs[i].info == nil
		}
		return funs[i].info.pos < funs[j].info.pos
	})

	for _, f := range funs {
		if f.info != nil {
			f.typeCheck(c, f.info.simpleTypeCheck)
		}
	}

	lets := []*Let{}
	for _, let := range c.lets {
		lets = append(lets, let)
	}

	for _, l := range lets {
		if l.info != nil {
			l.info.pos = c.getNextPos(l.info.size)
		}
	}

	putUvarint(bytes[saddr:saddr+8], sinfo.pos)

	for _, f := range funs {
		if f.info != nil && !f.info.inline {
			bytes = append(bytes, f.compile(c)...)
		}
	}

	sort.Slice(lets, func(i, j int) bool {
		if lets[i].info == nil || lets[j].info == nil {
			return lets[i].info == nil
		}
		return lets[i].info.pos < lets[j].info.pos
	})

	for _, l := range lets {
		if l.info != nil {
			bytes = append(bytes, l.staticCompile(c)...)
		}
	}

	return append(bytes, []uint8(c.strs)...)
}

func (l *Let) staticCompile(c *Ctx) []uint8 {
	if len(l.let.Exprs) != 1 {
		panic(fmt.Sprintf("let '%s' has to haves exactly one expr", l.let.Ident.Content))
	}
	expr := l.let.Exprs[0]
	var bytes []uint8
	number := expr.AsNumber()
	str := expr.AsString()
	if number != nil {
		num, err := strconv.ParseUint(number.Content, number.Base, number.Size*8)
		bytes = make([]uint8, l.info.size)
		if err != nil {
			panic(fmt.Sprintf("unable to convert '%s' to a number", number.Content))
		}
		putUvarint(bytes, num)
	} else if str != nil {
		c.pushStr(str.Content)
		ptr := c.getStr(str.Content)
		buf := []uint8{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		putUvarint(buf[0:8], ptr)
		putUvarint(buf[8:], uint64(len(str.Content)))
		bytes = append(bytes, buf...)
	} else {
		panic(fmt.Sprintf("let '%s' can only have a string or number expr", l.let.Ident.Content))
	}
	return bytes
}

func (c *Ctx) getNextPos(size uint64) uint64 {
	res := c.size
	c.size += size
	return res
}

func (f *Fun) compile(c *Ctx) []uint8 {
	bytes := []uint8{}

	if f.info.asm {
		bytes = f.compileAsm(c)
		if !f.info.inline {
			bytes = append(bytes, 3)
		}
	} else {
		lets := []*Let{}
		for _, let := range f.info.lets {
			lets = append(lets, let)
		}
		sort.Slice(lets, func(i, j int) bool {
			return lets[i].info.pos < lets[j].info.pos
		})
		for _, l := range lets {
			bytes = append(bytes, f.compileExprs(c, l.let.Exprs)...)
			buf := []uint8{}
			pos := f.info.pos + f.info.size - f.info.letSize + l.info.pos
			for off, size := range l.let.Typ.LoadSizes(c.types) {
				len := len(buf)
				switch size {
				case 0:
					continue
				case 1:
					buf = append(buf, 235, 0, 0, 0, 0, 0, 0, 0, 0)
				case 2:
					buf = append(buf, 236, 0, 0, 0, 0, 0, 0, 0, 0)
				case 4:
					buf = append(buf, 237, 0, 0, 0, 0, 0, 0, 0, 0)
				case 8:
					buf = append(buf, 238, 0, 0, 0, 0, 0, 0, 0, 0)
				case 16:
					buf = append(buf, 239, 0, 0, 0, 0, 0, 0, 0, 0)
				default:
					panic("invalid size")
				}
				putUvarint(buf[len+1:], pos+l.info.size-8-uint64(8*off))
			}
			bytes = append(bytes, buf...)
		}

		bytes = append(bytes, f.compileExprs(c, f.fun.Block.Exprs)...)
		if f.makeFunIdent(c) == c.start {
			bytes = append(bytes, 1)
		} else if !f.info.inline {
			bytes = append(bytes, 3)
		}
		bytes = append(bytes, make([]uint8, f.info.letSize, f.info.letSize)...)
	}
	return bytes
}

func (f *Fun) compileExprs(c *Ctx, exprs []parser.Expr) []uint8 {
	bytes := []uint8{}
	for i := 0; i < len(exprs); i++ {
		expr := exprs[i]
		ident := expr.AsIdent()
		call := expr.AsCall()
		number := expr.AsNumber()
		str := expr.AsString()
		ifel := expr.AsIf()
		while := expr.AsWhile()
		unwrap := expr.AsUnwrap()
		wrap := expr.AsWrap()
		addr := expr.AsAddr()
		ret := expr.AsReturn()
		if ident != nil {
			ident := ident.Content
			let := f.info.lets[ident]
			var pos uint64
			if let == nil {
				let = c.lets[ident]
				pos = let.info.pos
			} else {
				pos = f.info.pos + f.info.size - f.info.letSize + let.info.pos
			}
			buf := []uint8{}
			for off, size := range let.let.Typ.LoadSizes(c.types) {
				len := len(buf)
				switch size {
				case 0:
					continue
				case 1:
					buf = append(buf, 230, 0, 0, 0, 0, 0, 0, 0, 0)
				case 2:
					buf = append(buf, 231, 0, 0, 0, 0, 0, 0, 0, 0)
				case 4:
					buf = append(buf, 232, 0, 0, 0, 0, 0, 0, 0, 0)
				case 8:
					buf = append(buf, 233, 0, 0, 0, 0, 0, 0, 0, 0)
				case 16:
					buf = append(buf, 234, 0, 0, 0, 0, 0, 0, 0, 0)
				default:
					panic("invalid size")
				}
				putUvarint(buf[len+1:], pos+uint64(8*off))
			}
			bytes = append(bytes, buf...)
		} else if call != nil {
			ident := c.makeFunIdent(call.Ident.Content, call.Inputs, call.Outputs)
			fun := c.funs[ident]
			if fun.info.inline {
				bytes = append(bytes, fun.compile(c)...)
			} else {
				buf := []uint8{229, 0, 0, 0, 0, 0, 0, 0, 0}
				putUvarint(buf[1:], fun.info.pos)
				bytes = append(bytes, buf...)

			}
		} else if number != nil {
			num, err := strconv.ParseUint(number.Content, number.Base, number.Size*8)
			if err != nil {
				panic(fmt.Sprintf("unable to convert '%s' to a number", number.Content))
			}
			var buf []uint8
			switch number.Size {
			case 1:
				buf = []uint8{10, 0}
			case 2:
				buf = []uint8{11, 0, 0}
			case 4:
				buf = []uint8{12, 0, 0, 0, 0}
			case 8:
				buf = []uint8{13, 0, 0, 0, 0, 0, 0, 0, 0}
			default:
				panic("invalid size")
			}
			putUvarint(buf[1:], num)
			bytes = append(bytes, buf...)
		} else if str != nil {
			ptr := c.getStr(str.Content)
			buf := []uint8{13, 0, 0, 0, 0, 0, 0, 0, 0, 13, 0, 0, 0, 0, 0, 0, 0, 0}
			putUvarint(buf[1:9], ptr)
			putUvarint(buf[10:], uint64(len(str.Content)))
			bytes = append(bytes, buf...)
		} else if ifel != nil {
			bytes = append(bytes, f.compileExprs(c, ifel.Con)...)

			buf := []uint8{226, 0, 0, 0, 0, 0, 0, 0, 0}
			putUvarint(buf[1:], f.sizeOfExprs(c, ifel.Else)+18)
			bytes = append(bytes, buf...)
			bytes = append(bytes, f.compileExprs(c, ifel.Else)...)

			buf = []uint8{221, 0, 0, 0, 0, 0, 0, 0, 0}
			putUvarint(buf[1:], f.sizeOfExprs(c, ifel.Exprs)+9)
			bytes = append(bytes, buf...)
			bytes = append(bytes, f.compileExprs(c, ifel.Exprs)...)
		} else if while != nil {
			bytes = append(bytes, f.compileExprs(c, while.Con)...)

			buf := []uint8{226, 0, 0, 0, 0, 0, 0, 0, 0}
			putUvarint(buf[1:], 18)
			bytes = append(bytes, buf...)

			buf = []uint8{221, 0, 0, 0, 0, 0, 0, 0, 0}
			putUvarint(buf[1:], f.sizeOfExprs(c, while.Exprs)+18)
			bytes = append(bytes, buf...)

			bytes = append(bytes, f.compileExprs(c, while.Exprs)...)

			buf = []uint8{222, 0, 0, 0, 0, 0, 0, 0, 0}
			putUvarint(buf[1:], f.sizeOfExprs(c, while.Exprs)+f.sizeOfExprs(c, while.Con)+18)
			bytes = append(bytes, buf...)
		} else if unwrap != nil {
		} else if wrap != nil {
		} else if addr != nil {
			var pos uint64
			if addr.Ident != nil {
				ident := addr.Ident.Content
				let := f.info.lets[ident]
				if let == nil {
					let = c.lets[ident]
					pos = let.info.pos
				} else {
					pos = f.info.pos + f.info.size - f.info.letSize + let.info.pos
				}
			} else {
				call = addr.Call
				ident := c.makeFunIdent(call.Ident.Content, call.Inputs, call.Outputs)
				pos = c.funs[ident].info.pos
			}
			buf := []uint8{13, 0, 0, 0, 0, 0, 0, 0, 0}
			putUvarint(buf[1:], pos)
			bytes = append(bytes, buf...)
		} else if ret != nil {
			bytes = append(bytes, 3)
		} else {
			panic("unreachable")
		}
	}

	return bytes
}

func (c *Ctx) pushStr(s string) {
	if strings.Index(c.strs, s) == -1 {
		c.strs += s
	}
}

func (c *Ctx) getStr(s string) uint64 {
	return uint64(strings.Index(c.strs, s)) + c.size
}

func putUvarint(buf []uint8, x uint64) {
	i := 0
	for x > 0xff {
		buf[i] = uint8(x)
		x >>= 8
		i++
	}
	buf[i] = uint8(x)
}
