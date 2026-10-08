// qr.go — minimal QR encoder for the /mobile and /session dialogs: byte
// mode, EC level L, versions 1-4 (≤78 bytes — covers every URL we print),
// mask chosen by the N1/N2 penalty rules.

package repl

import "strings"

var (
	qrDataCapL = []int{19, 34, 55, 78} // byte-mode data codewords, v1-4 @ L
	qrEcLenL   = []int{7, 10, 15, 20}  // ec codewords per (single) block, v1-4 @ L
	qrAlign    = []int{0, 18, 22, 26}  // second alignment center per version
)

var qrExp, qrLog [512]byte

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		qrExp[i] = byte(x)
		qrLog[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11d
		}
	}
	for i := 255; i < 512; i++ {
		qrExp[i] = qrExp[i-255]
	}
}

func qrMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return qrExp[int(qrLog[a])+int(qrLog[b])]
}

// qrDivisor is the Reed-Solomon generator polynomial of degree deg
// (Nayuki's reedSolomonComputeDivisor).
func qrDivisor(deg int) []byte {
	res := make([]byte, deg)
	res[deg-1] = 1
	root := byte(1)
	for i := 0; i < deg; i++ {
		for j := 0; j < deg; j++ {
			res[j] = qrMul(res[j], root)
			if j+1 < deg {
				res[j] ^= res[j+1]
			}
		}
		root = qrMul(root, 2)
	}
	return res
}

func qrRemainder(data []byte, div []byte) []byte {
	res := make([]byte, len(div))
	for _, b := range data {
		f := b ^ res[0]
		copy(res, res[1:])
		res[len(res)-1] = 0
		for j := range res {
			res[j] ^= qrMul(div[j], f)
		}
	}
	return res
}

type qrBitBuf struct {
	bits []bool
}

func (b *qrBitBuf) append(v, n int) {
	for i := n - 1; i >= 0; i-- {
		b.bits = append(b.bits, v&(1<<i) != 0)
	}
}
func (b *qrBitBuf) bytes() []byte {
	out := make([]byte, (len(b.bits)+7)/8)
	for i, bit := range b.bits {
		if bit {
			out[i>>3] |= 0x80 >> (i & 7)
		}
	}
	return out
}

// qrMatrix builds the module matrix for text, or nil if it doesn't fit.
func qrMatrix(text string) [][]bool {
	data := []byte(text)
	ver := -1
	for v := 0; v < 4; v++ {
		if len(data) <= qrDataCapL[v] {
			ver = v + 1
			break
		}
	}
	if ver < 0 {
		return nil
	}
	capN := qrDataCapL[ver-1]
	// byte mode: 0100 + count(8) + data + terminator + pad
	var bb qrBitBuf
	bb.append(4, 4)
	bb.append(len(data), 8)
	for _, c := range data {
		bb.append(int(c), 8)
	}
	for i := 0; i < 4 && len(bb.bits) < capN*8; i++ {
		bb.append(0, 1)
	}
	for len(bb.bits)%8 != 0 {
		bb.append(0, 1)
	}
	cw := bb.bytes()
	pads := []int{0xEC, 0x11}
	for i := 0; len(cw) < capN; i++ {
		cw = append(cw, byte(pads[i%2]))
	}
	cw = append(cw, qrRemainder(cw, qrDivisor(qrEcLenL[ver-1]))...)

	var best [][]bool
	bestPen := -1
	for mask := 0; mask < 8; mask++ {
		m := qrPlace(cw, ver, mask)
		if p := qrPenalty(m); bestPen < 0 || p < bestPen {
			bestPen, best = p, m
		}
	}
	return best
}

// qrFormatL holds the 15-bit format info for EC level L, indexed by mask.
var qrFormatL = [8]int{
	0b111011111000100, 0b111001011110011, 0b111110110101010, 0b111100010011101,
	0b110011000101111, 0b110001100011000, 0b110110001000001, 0b110100101110110,
}

func qrMask(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	}
	return ((x+y)%2+x*y%3)%2 == 0
}

func qrPlace(cw []byte, ver, mask int) [][]bool {
	size := ver*4 + 17
	m := make([][]bool, size)
	fn := make([][]bool, size)
	for i := range m {
		m[i] = make([]bool, size)
		fn[i] = make([]bool, size)
	}
	set := func(x, y int, v bool) { m[y][x] = v; fn[y][x] = true }
	finder := func(cx, cy int) {
		for dy := -1; dy <= 7; dy++ {
			for dx := -1; dx <= 7; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= size || y >= size {
					continue
				}
				on := dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6 &&
					(dx == 0 || dx == 6 || dy == 0 || dy == 6 ||
						(dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4))
				set(x, y, on)
			}
		}
	}
	finder(0, 0)
	finder(size-7, 0)
	finder(0, size-7)
	for i := 8; i < size-8; i++ {
		set(6, i, i%2 == 0)
		set(i, 6, i%2 == 0)
	}
	if ver > 1 { // single alignment pattern at (a,a)
		a := qrAlign[ver-1]
		for dy := -2; dy <= 2; dy++ {
			for dx := -2; dx <= 2; dx++ {
				set(a+dx, a+dy, dx == -2 || dx == 2 || dy == -2 || dy == 2 || (dx == 0 && dy == 0))
			}
		}
	}
	// format info (drawn pre-mask; values for the chosen mask)
	fb := qrFormatL[mask]
	bit := func(i int) bool { return fb&(1<<i) != 0 }
	for i := 0; i <= 5; i++ {
		set(8, i, bit(i))
	}
	set(8, 7, bit(6))
	set(8, 8, bit(7))
	set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		set(size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		set(8, size-15+i, bit(i))
	}
	set(8, size-8, true) // dark module
	// data bits, zigzag from bottom-right
	i := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = size - 1 - vert
				}
				if fn[y][x] {
					continue
				}
				var v bool
				if i < len(cw)*8 {
					v = cw[i>>3]&(0x80>>(i&7)) != 0
					i++
				}
				m[y][x] = v != qrMask(mask, x, y)
			}
		}
	}
	return m
}

// qrPenalty scores runs (N1) + 2x2 blocks (N2) — enough to pick a readable
// mask for short text.
func qrPenalty(m [][]bool) int {
	size := len(m)
	pen := 0
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			for _, d := range [][2]int{{1, 0}, {0, 1}} {
				run := 1
				for x+d[0]*run < size && y+d[1]*run < size &&
					m[y+d[1]*run][x+d[0]*run] == m[y][x] {
					run++
				}
				if run >= 5 && (d[0] == 1 || x == 0 || m[y][x-1] != m[y][x]) &&
					(d[1] == 0 || y == 0 || m[y-1][x] != m[y][x]) {
					pen += run - 2
				}
			}
			if x+1 < size && y+1 < size &&
				m[y][x] == m[y][x+1] && m[y][x] == m[y+1][x] && m[y][x] == m[y+1][x+1] {
				pen += 3
			}
		}
	}
	return pen
}

// qrText renders the matrix as two-space blocks / full-block runes with a
// quiet zone — scannable on both dark and light terminals.
func qrText(text string) []string {
	m := qrMatrix(text)
	if m == nil {
		return nil
	}
	size := len(m)
	var rows []string
	for y := -1; y <= size; y++ {
		var sb strings.Builder
		for x := -1; x <= size; x++ {
			on := x >= 0 && y >= 0 && x < size && y < size && m[y][x]
			if on {
				sb.WriteString("██")
			} else {
				sb.WriteString("  ")
			}
		}
		rows = append(rows, "  "+sb.String())
	}
	return rows
}
