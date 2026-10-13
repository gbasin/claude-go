// cellseg_shim.mjs — pure-JS implementation of Bun.ant.CellSegmenter for
// stock Bun, so the unpacked real source runs its renderer without the
// internal fork. Preloaded via `bun --preload`.
//
// Contract (reverse-engineered from chunk-06azbynj.js / chunk-t58kj6z7.js):
//   new CellSegmenter({ambiguousIsNarrow, substitute, screen})
//   fields: graphemes[], sgrKeys[], sgrCloseKeys[], uris[]
//   segment(text, cells, runs, reorder) -> count | -needed
//     cells[2i]   = graphemes index
//     cells[2i+1] = runIdx<<10 | flags<<8 | cellWidth   (flag bit8 = tab)
//     runs[2r]    = sgrKeys index, runs[2r+1] = uris index (0 = none)
//   paint(screenCells, w, x, y, cells, count, _, charIndices, runWords)
//     screenCells[2*(y*w+col)]   = charPool index (charIndices[graphemeIdx])
//     screenCells[2*(y*w+col)+1] = runWords[runIdx] | width2
//       width2: 0 narrow, 1 wide, 2 spacerTail, 3 spacerHead
//     -> damage scalar: xHi<<36 | xLo<<20 | endCol
//   setCell(screenCells, w, x, y, charIdx, packedWord) -> damage
//   sgrKeys[r]  = "\x00"-joined single-param CSI SGR open sequences
//   sgrCloseKeys[r] = parallel "\x00"-joined close sequences
//   graphemes[0] must be " " (emptyCharIndex) and [1] "" (spacerCharIndex)

const SEG = new Intl.Segmenter("en", { granularity: "grapheme" });

// --- width table: TR11 Wide/Fullwidth + terminal-2-cell emoji blocks ---
// (same ranges the Go replica uses)
const WIDE = [
	[0x1100,0x115f],[0x231a,0x231b],[0x2329,0x232a],[0x23e9,0x23ec],
	[0x23f0,0x23f0],[0x23f3,0x23f3],[0x25fd,0x25fe],[0x2614,0x2615],
	[0x2648,0x2653],[0x267f,0x267f],[0x2693,0x2693],[0x26a1,0x26a1],
	[0x26aa,0x26ab],[0x26bd,0x26be],[0x26c4,0x26c5],[0x26ce,0x26ce],
	[0x26d4,0x26d4],[0x26ea,0x26ea],[0x26f2,0x26f3],[0x26f5,0x26f5],
	[0x26fa,0x26fa],[0x26fd,0x26fd],[0x2705,0x2705],[0x270a,0x270b],
	[0x2728,0x2728],[0x274c,0x274c],[0x274e,0x274e],[0x2753,0x2755],
	[0x2757,0x2757],[0x2795,0x2797],[0x27b0,0x27b0],[0x27bf,0x27bf],
	[0x2b1b,0x2b1c],[0x2b50,0x2b50],[0x2b55,0x2b55],[0x2e80,0x2fdf],
	[0x2ff0,0x2ffb],[0x3000,0x303e],[0x3041,0x3096],[0x3099,0x30ff],
	[0x3105,0x312f],[0x3131,0x318e],[0x3190,0x31e3],[0x31f0,0x321e],
	[0x3220,0x3247],[0x3250,0x4dbf],[0x4e00,0xa4c6],[0xa4d0,0xa62b],
	[0xa640,0xa6f2],[0xa960,0xa97c],[0xac00,0xd7a3],[0xf900,0xfaff],
	[0xfe10,0xfe19],[0xfe30,0xfe52],[0xfe54,0xfe66],[0xfe68,0xfe6b],
	[0xff00,0xff60],[0xffe0,0xffe6],[0x16fe0,0x16fe4],[0x17000,0x18cd5],
	[0x18d00,0x18d08],[0x1aff0,0x1afff],[0x1b000,0x1b152],[0x1b164,0x1b167],
	[0x1b170,0x1b2fb],[0x1f004,0x1f004],[0x1f0cf,0x1f0cf],[0x1f18e,0x1f18e],
	[0x1f191,0x1f19a],[0x1f200,0x1f202],[0x1f210,0x1f23b],[0x1f240,0x1f248],
	[0x1f250,0x1f251],[0x1f260,0x1f265],[0x1f300,0x1f6ff],[0x1f900,0x1f9ff],
	[0x1fa70,0x1faff],[0x20000,0x2fffd],[0x30000,0x3fffd],
];
function cpWide(cp) {
	let lo = 0, hi = WIDE.length - 1;
	while (lo <= hi) {
		const m = (lo + hi) >> 1;
		if (cp < WIDE[m][0]) hi = m - 1;
		else if (cp > WIDE[m][1]) lo = m + 1;
		else return true;
	}
	return false;
}
// combining / zero-width ranges
const ZERO = [
	[0x0300,0x036f],[0x0483,0x0489],[0x0591,0x05bd],[0x05bf,0x05bf],
	[0x05c1,0x05c2],[0x05c4,0x05c5],[0x05c7,0x05c7],[0x0610,0x061a],
	[0x064b,0x065f],[0x0670,0x0670],[0x06d6,0x06dc],[0x06df,0x06e4],
	[0x06e7,0x06e8],[0x06ea,0x06ed],[0x0e31,0x0e31],[0x0e34,0x0e3a],
	[0x0e47,0x0e4e],[0x1ab0,0x1aff],[0x1dc0,0x1dff],[0x20d0,0x20ff],
	[0xfe00,0xfe0f],[0xfe20,0xfe2f],[0x1f3fb,0x1f3ff],[0xe0100,0xe01ef],
	[0x200b,0x200f],[0x202a,0x202e],[0x2060,0x2064],[0xfeff,0xfeff],
	[0x00ad,0x00ad],[0x1680,0x1680], // soft hyphen / ogham space -> 0? keep narrow? treat as 0
];
function cpZero(cp) {
	let lo = 0, hi = ZERO.length - 1;
	while (lo <= hi) {
		const m = (lo + hi) >> 1;
		if (cp < ZERO[m][0]) hi = m - 1;
		else if (cp > ZERO[m][1]) lo = m + 1;
		else return true;
	}
	return false;
}

// --- SGR attribute groups: param -> {group, openSeq, closeSeq} ---
// Each param opens a single-param sequence; a new param in the same group
// replaces the previous one (and its close).
function grp(code, close) { return { g: code, close }; }
const PARAM_GROUP = new Map();
function reg(close, ...codes) { for (const c of codes) PARAM_GROUP.set(c, grp(c, close)); }
reg("\x1b[22m", 1, 2);            // bold/dim
reg("\x1b[23m", 3);               // italic
reg("\x1b[24m", 4, 21);           // underline styles
reg("\x1b[25m", 5, 6);            // blink
reg("\x1b[27m", 7);               // inverse
reg("\x1b[28m", 8);               // conceal
reg("\x1b[29m", 9);               // strikethrough
reg("\x1b[39m", 30,31,32,33,34,35,36,37,90,91,92,93,94,95,96,97); // fg
reg("\x1b[49m", 40,41,42,43,44,45,46,47,100,101,102,103,104,105,106,107); // bg
reg("\x1b[59m", 58);              // underline color (extended args follow)
reg("\x1b[10m", 10,11,12);        // fonts
reg("\x1b[55m", 53);              // overline
// param 0 = reset all, 38/48 take extended args (handled in parser)

class CellSegmenter {
	constructor(opts) {
		this.opts = opts || {};
		this.graphemes = [" ", ""];      // [0]=emptyCharIndex " ", [1]=spacerCharIndex ""
		this.sgrKeys = [""];             // run 0 = default style
		this.sgrCloseKeys = [""];
		this.uris = [""];                // uri 0 = none
		this._gMap = new Map([[" ", 0], ["", 1]]);
		this._sgrMap = new Map([["", 0]]);
		this._uriMap = new Map();
		this._scr = Object.assign({
			widthMask: 3, narrow: 0, wide: 1, spacerTail: 2, spacerHead: 3,
			emptyCharIndex: 0, spacerCharIndex: 1, emptyWord: 0, tabWidth: 8,
		}, this.opts.screen || {});
		this._subRanges = (this.opts.substitute || []).map(([a, b]) => [a, b]);
	}
	resetNative() {
		this.graphemes.length = 2; this.sgrKeys.length = 1;
		this.sgrCloseKeys.length = 1; this.uris.length = 1;
		this._gMap = new Map([[" ", 0], ["", 1]]);
		this._sgrMap = new Map([["", 0]]);
		this._uriMap = new Map();
	}
	_gi(g) {
		let i = this._gMap.get(g);
		if (i === undefined) { i = this.graphemes.length; this.graphemes.push(g); this._gMap.set(g, i); }
		return i;
	}
	_si(open, close) {
		const k = open + "|" + close;
		let i = this._sgrMap.get(k);
		if (i === undefined) {
			i = this.sgrKeys.length;
			this.sgrKeys.push(open); this.sgrCloseKeys.push(close); this._sgrMap.set(k, i);
		}
		return i;
	}
	_ui(u) {
		if (!u) return 0;
		let i = this._uriMap.get(u);
		if (i === undefined) { i = this.uris.length; this.uris.push(u); this._uriMap.set(u, i); }
		return i;
	}
	_inSub(cp) {
		for (const [a, b] of this._subRanges) if (cp >= a && cp <= b) return true;
		return false;
	}
	// grapheme cell width: wide=2, zero-width marks=0, else 1
	_gw(g) {
		if (g === "\t") return -1; // tab marker
		let w = 0;
		for (const ch of g) {
			const cp = ch.codePointAt(0);
			if (this._inSub(cp)) continue;
			if (cpZero(cp)) continue;
			if (cpWide(cp)) { w += 2; continue; }
			if (cp < 0x20 || cp === 0x7f) continue; // control -> invis
			w += 1;
		}
		if (w === 0 && g.length > 0) {
			// all-zero grapheme: occupies no cell but still emitted
			return 0;
		}
		return w;
	}

	// segment(text, cells, runs, reorder) — parse ANSI + graphemes.
	// cells needs 2*maxGraphemes ints; runs needs 2*maxRuns ints.
	// Returns count, or -(requiredCapacity) when arrays are too small.
	segment(text, cells, runs, reorder) {
		// pass 1: tokenize into graphemes + run breaks (style/uri changes)
		const gph = [];    // grapheme strings
		const flags = [];  // per-grapheme flag byte (bit0 -> cells flag bit8? no: bit8 word = tab)
		const widths = []; // per-grapheme cell width
		const rkeys = [];  // per-grapheme sgrKey index
		const ruris = [];  // per-grapheme uri index
		let sgr = [];      // active [{param,openSeq,closeSeq}] — order preserved
		let uri = 0;

		const emitG = (g) => {
			const w = g === "\t" ? -1 : this._gw(g);
			if (w === 0 && g !== "\t" && g.length && g.codePointAt(0) !== undefined && cpCtrl(g)) {
				// still emits a cell
			}
			gph.push(g);
			const o = sgr.join("\x00"), c = sgr.map(x => x.close).join("\x00");
			rkeys.push(this._si(o, c));
			ruris.push(uri);
			if (g === "\t") { flags.push(1); widths.push(0); }
			else { flags.push(0); widths.push(w); }
		};

		let i = 0;
		const n = text.length;
		while (i < n) {
			const ch = text[i];
			if (ch === "\x1b") {
				// CSI
				if (text[i + 1] === "[") {
					const m = /^\x1b\[([0-9;:]*)([A-Za-z])/.exec(text.slice(i));
					if (m) {
						if (m[2] === "m") {
							sgr = applySgr(sgr, m[1]);
						}
						i += m[0].length;
						continue;
					}
				}
				// OSC: ESC ] ... (BEL | ESC \)
				if (text[i + 1] === "]") {
					const m = /^\x1b\]([^\x07\x1b]*)(?:\x07|\x1b\\)/.exec(text.slice(i));
					if (m) {
						const body = m[1];
						if (body.startsWith("8;")) {
							// OSC 8 hyperlink: 8;params;uri
							const semi = body.indexOf(";", 2);
							const u = semi >= 0 ? body.slice(semi + 1) : "";
							uri = u ? this._ui(u) : 0;
						}
						i += m[0].length;
						continue;
					}
				}
				// any other escape: consume introducer + final
				const m2 = /^\x1b[^\x1b]*?([@-~])/.exec(text.slice(i));
				if (m2) { i += m2[0].length; continue; }
				i++;
				continue;
			}
			// grapheme cluster
			const segIt = SEG.segment(text.slice(i));
			const g = segIt.containing(0).segment;
			emitG(g);
			i += g.length;
		}

		// build run table: consecutive graphemes with same (sgr,uri)
		const runOf = new Array(gph.length);
		const runList = []; // [sgrKeyIdx, uriIdx]
		let cur = -1;
		for (let k = 0; k < gph.length; k++) {
			const key = rkeys[k] + "|" + ruris[k];
			if (cur < 0 || runList[cur][0] !== rkeys[k] || runList[cur][1] !== ruris[k]) {
				runList.push([rkeys[k], ruris[k]]);
				cur = runList.length - 1;
			}
			runOf[k] = cur;
		}

		// capacity check
		if (cells.length < 2 * gph.length || runs.length < 2 * runList.length) {
			return -Math.max(gph.length, runList.length, 1);
		}
		for (let k = 0; k < gph.length; k++) {
			cells[2 * k] = this._gi(gph[k]);
			cells[2 * k + 1] = (runOf[k] << 10) | (flags[k] << 8) | (widths[k] & 255);
		}
		for (let r = 0; r < runList.length; r++) {
			runs[2 * r] = runList[r][0];
			runs[2 * r + 1] = runList[r][1];
		}
		return gph.length;
	}

	// paint(screenCells, w, x, y, cells, count, _, charIndices, runWords)
	paint(screen, w, x, y, cells, count, _u, charIndices, runWords) {
		const S = this._scr;
		let col = x;
		let xLo = -1, xHi = -1;
		for (let k = 0; k < count; k++) {
			const gi = cells[2 * k];
			const word = cells[2 * k + 1];
			const runIdx = word >>> 10;
			const isTab = (word & 256) !== 0;
			let cw = word & 255;
			const styleWord = runWords[runIdx] | 0;
			if (isTab) {
				const adv = S.tabWidth - (col % S.tabWidth);
				for (let t = 0; t < adv && col + t < w; t++) {
					const p = (y * w + col + t) << 1;
					screen[p] = S.emptyCharIndex;
					screen[p + 1] = styleWord | S.narrow;
				}
				col += adv;
				if (xLo < 0) xLo = col - adv;
				xHi = Math.max(xHi, col - 1);
				continue;
			}
			if (cw === 0) continue;
			if (col + cw > w) break; // clip at right edge
			const p = (y * w + col) << 1;
			if (cw >= 2) {
				screen[p] = charIndices[gi];
				screen[p + 1] = styleWord | S.wide;
				for (let t = 1; t < cw; t++) {
					const q = p + (t << 1);
					screen[q] = S.spacerCharIndex;
					screen[q + 1] = styleWord | S.spacerTail;
				}
			} else {
				screen[p] = charIndices[gi];
				screen[p + 1] = styleWord | S.narrow;
			}
			if (xLo < 0) xLo = col;
			col += cw;
			xHi = Math.max(xHi, col - 1);
		}
		if (xLo < 0) return 0;
		return (xHi + 1) * 68719476736 + xLo * 1048576 + col;
	}

	setCell(screen, w, x, y, charIdx, packedWord) {
		if (x < 0 || y < 0) return 0;
		const p = (y * w + x) << 1;
		screen[p] = charIdx;
		screen[p + 1] = packedWord;
		const w2 = packedWord & this._scr.widthMask;
		const span = w2 === 1 ? 2 : 1;
		return (x + span) * 68719476736 + x * 1048576 + (x + span);
	}
}

function cpCtrl() { return false; }

// applySgr: maintain active attribute list (order-preserved), params
// normalized to single-param sequences per real's Zx regex.
function applySgr(active, paramStr) {
	if (paramStr === "" || paramStr === "0") return [];
	const parts = paramStr.split(";").map(s => s === "" ? 0 : parseInt(s, 10));
	for (let i = 0; i < parts.length; i++) {
		const p = parts[i];
		if (p === 0) { active = []; continue; }
		if (p === 38 || p === 48) {
			// extended color: 5;n or 2;r;g;b
			let seq;
			if (parts[i + 1] === 5) { seq = `\x1b[${p};5;${parts[i + 2]}m`; i += 2; }
			else if (parts[i + 1] === 2) { seq = `\x1b[${p};2;${parts[i + 2]};${parts[i + 3]};${parts[i + 4]}m`; i += 4; }
			else continue;
			const close = p === 38 ? "\x1b[39m" : "\x1b[49m";
			active = active.filter(a => a.g !== p);
			active.push({ g: p, open: seq, close });
			continue;
		}
		if (p === 58) { // underline color: 5;n or 2;r;g;b
			let seq;
			if (parts[i + 1] === 5) { seq = `\x1b[58;5;${parts[i + 2]}m`; i += 2; }
			else if (parts[i + 1] === 2) { seq = `\x1b[58;2;${parts[i + 2]};${parts[i + 3]};${parts[i + 4]}m`; i += 4; }
			else continue;
			active = active.filter(a => a.g !== 58);
			active.push({ g: 58, open: seq, close: "\x1b[59m" });
			continue;
		}
		const meta = PARAM_GROUP.get(p);
		if (!meta) continue; // unknown param — real's parser may still track; drop
		active = active.filter(a => a.g !== meta.g);
		active.push({ g: meta.g, open: `\x1b[${p}m`, close: meta.close });
	}
	return active;
}

// install
if (typeof Bun !== "undefined") {
	Bun.ant = Bun.ant || {};
	Bun.ant.CellSegmenter = CellSegmenter;
	Bun.ant.getPeerPid = Bun.ant.getPeerPid || (() => 0);
	Bun.ant.getPeerUid = Bun.ant.getPeerUid || (() => 0);
	Bun.ant.memoryPressureLevel = Bun.ant.memoryPressureLevel || (() => 0);
	Bun.ant.waitForUrlEvent = Bun.ant.waitForUrlEvent || (() => null);
}
export { CellSegmenter };
